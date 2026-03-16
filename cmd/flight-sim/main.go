// flight-sim: Simulates a full drone flight lifecycle near your TAPs.
//
// Scenario: DJI Air 2S takes off ~2km NE of TAPs, approaches, circles,
// then the operator kills RemoteID. The system should:
//   1. Track with GPS + RSSI (calibration learns model TX power)
//   2. Remember identity (MAC -> model mapping)
//   3. Continue tracking via RSSI-only after RemoteID goes dark
//   4. Use live-calibrated RSSI_0 for accurate range rings (not generic)
//
// What to watch on the dashboard:
//   Phase 1 (APPROACH):  Drone appears as "DJI Air 2S", GPS dot moving toward TAPs
//   Phase 2 (CIRCLE):    Drone orbits near TAPs, range rings tighten with live calibration
//   Phase 3 (DARK):      GPS disappears but range rings PERSIST with model-calibrated RSSI
//   Phase 4 (DEPART):    Signal weakens, range rings expand, eventually goes LOST
//
// Usage:
//   go run ./cmd/flight-sim/
package main

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	pb "github.com/K13094/skylens/proto"
	"google.golang.org/protobuf/proto"
)

// ============================================================
// Drone identity
// ============================================================
const (
	droneSerial = "1581F5A8K2294000B04F" // DJI Air 2S (F5A prefix)
	droneMAC    = "60:60:1F:AA:11:22"    // DJI OUI
	droneSSID   = "DJI-AIR2S-SIM"        // DJI SSID pattern
)

// ============================================================
// TAP locations (your real deployment)
// ============================================================
type tapInfo struct {
	ID  string
	Lat float64
	Lon float64
}

var taps = []tapInfo{
	{"tap-001", 18.253187, -65.644547},
	{"tap-002", 18.252459, -65.640278},
	{"tap-003", 18.252459, -65.640278},
}

// ============================================================
// Flight parameters
// ============================================================
const (
	// Timing
	updateInterval = 1500 * time.Millisecond // Detection every 1.5s
	tapDelay       = 80 * time.Millisecond   // Stagger between TAP detections

	// RSSI simulation: RSSI = rssi0 - 10*n*log10(distance) + noise
	simRSSI0   = -17.7 // Air 2S RSSI_0 at n=1.8 (BaseRSSI0 + offset = -26.8 + 9.1)
	simN       = 1.8   // Path loss exponent (must match intel.DefaultPathLossN)
	simNoiseSd = 3.0   // RSSI noise std dev (dBm)

	// Flight geometry
	startLat = 18.268 // ~2km NE of TAPs
	startLon = -65.627
	endLat   = 18.238 // ~2km SW of TAPs
	endLon   = -65.660

	circleCenter_lat = 18.2545 // Near TAP centroid
	circleCenter_lon = -65.6420
	circleRadiusM    = 420.0 // Circle radius in meters

	// Operator stays at takeoff point
	operatorLat = startLat
	operatorLon = startLon
	operatorAlt = float32(5.0) // ground level

	// Drone altitude
	cruiseAltM = float32(120.0)
)

// ============================================================
// Flight phases
// ============================================================
type phase int

const (
	phaseApproach    phase = iota // 0-45s:  RemoteID ON, approaching from 2km
	phaseCircle                   // 45-120s: RemoteID ON, circling at 400m (calibration)
	phaseDarkCircle               // 120-180s: RemoteID OFF, still circling (identity recall)
	phaseDarkDepart               // 180-240s: RemoteID OFF, departing to 2km (signal fading)
)

func (p phase) String() string {
	switch p {
	case phaseApproach:
		return "APPROACH (RemoteID ON)"
	case phaseCircle:
		return "CIRCLE (RemoteID ON - building calibration)"
	case phaseDarkCircle:
		return "DARK CIRCLE (RemoteID OFF - identity recall)"
	case phaseDarkDepart:
		return "DARK DEPART (RemoteID OFF - signal fading)"
	}
	return "?"
}

func (p phase) hasRemoteID() bool {
	return p == phaseApproach || p == phaseCircle
}

func getPhase(elapsed float64) phase {
	switch {
	case elapsed < 45:
		return phaseApproach
	case elapsed < 120:
		return phaseCircle
	case elapsed < 180:
		return phaseDarkCircle
	default:
		return phaseDarkDepart
	}
}

// ============================================================
// Position calculation
// ============================================================

// metersToLatDeg converts meters to latitude degrees
func metersToLatDeg(m float64) float64 { return m / 111000.0 }

// metersToLonDeg converts meters to longitude degrees at a given latitude
func metersToLonDeg(m, lat float64) float64 {
	return m / (111000.0 * math.Cos(lat*math.Pi/180.0))
}

// dronePosition returns (lat, lon, heading, speed) for a given elapsed time
func dronePosition(elapsed float64) (lat, lon, heading, speed float64) {
	p := getPhase(elapsed)

	switch p {
	case phaseApproach:
		// Linear interpolation from start to circle entry
		t := elapsed / 45.0 // 0 -> 1 over 45 seconds
		entryLat := circleCenter_lat + metersToLatDeg(circleRadiusM) // Top of circle
		entryLon := circleCenter_lon
		lat = startLat + (entryLat-startLat)*t
		lon = startLon + (entryLon-startLon)*t
		heading = 225.0 // SW approach
		speed = 35.0    // m/s

	case phaseCircle, phaseDarkCircle:
		// Circular orbit (counter-clockwise)
		var circleTime float64
		if p == phaseCircle {
			circleTime = elapsed - 45.0 // 0-75s of circle
		} else {
			circleTime = elapsed - 45.0 // Continue from same point
		}
		// Full circle in ~75 seconds -> angular velocity
		angularSpeed := 2 * math.Pi / 75.0 // rad/s
		angle := math.Pi/2 - angularSpeed*circleTime // Start at top, go counter-clockwise
		lat = circleCenter_lat + metersToLatDeg(circleRadiusM)*math.Sin(angle)
		lon = circleCenter_lon + metersToLonDeg(circleRadiusM, circleCenter_lat)*math.Cos(angle)
		// Heading tangent to circle (perpendicular to radius, counter-clockwise)
		heading = math.Mod(360-(angle*180/math.Pi-90), 360)
		speed = circleRadiusM * angularSpeed // ~35 m/s

	case phaseDarkDepart:
		// Linear departure from circle bottom to end point
		t := (elapsed - 180.0) / 60.0 // 0 -> 1 over 60 seconds
		if t > 1 {
			t = 1
		}
		// Depart from bottom of circle
		departLat := circleCenter_lat - metersToLatDeg(circleRadiusM)
		departLon := circleCenter_lon
		lat = departLat + (endLat-departLat)*t
		lon = departLon + (endLon-departLon)*t
		heading = 225.0 // SW departure
		speed = 30.0 + 5.0*t // Accelerating away
	}

	return
}

// haversineM returns distance in meters between two GPS coords
func haversineM(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000
	p1 := lat1 * math.Pi / 180
	p2 := lat2 * math.Pi / 180
	dp := (lat2 - lat1) * math.Pi / 180
	dl := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// simulateRSSI returns realistic RSSI for a given distance
func simulateRSSI(distanceM float64) int32 {
	if distanceM < 1 {
		distanceM = 1
	}
	rssi := simRSSI0 - 10*simN*math.Log10(distanceM)
	// Add Gaussian noise
	noise := rand.NormFloat64() * simNoiseSd
	rssi += noise
	// Clamp
	if rssi > -25 {
		rssi = -25
	}
	if rssi < -100 {
		rssi = -100
	}
	return int32(math.Round(rssi))
}

// ============================================================
// Detection building
// ============================================================

func buildDetection(tap tapInfo, elapsed float64) *pb.Detection {
	lat, lon, heading, speed := dronePosition(elapsed)
	p := getPhase(elapsed)
	distToTap := haversineM(lat, lon, tap.Lat, tap.Lon)
	rssi := simulateRSSI(distToTap)

	det := &pb.Detection{
		TapId:      tap.ID,
		MacAddress: droneMAC,
		Rssi:       rssi,
		Channel:    149, // 5GHz WiFi
		Confidence: 0.85,
	}

	if p.hasRemoteID() {
		// === RemoteID ON: full telemetry ===
		det.Identifier = droneSerial
		det.SerialNumber = droneSerial
		det.Latitude = lat
		det.Longitude = lon
		det.AltitudeGeodetic = cruiseAltM
		det.HeightAgl = cruiseAltM
		det.Speed = float32(speed)
		det.Heading = float32(heading)
		det.VerticalSpeed = 0
		det.OperatorLatitude = operatorLat
		det.OperatorLongitude = operatorLon
		det.OperatorAltitude = operatorAlt
		det.Manufacturer = "RemoteID"
		det.Designation = "WiFi RemoteID broadcast"
		det.Source = pb.DetectionSource_SOURCE_TSHARK_REMOTEID
		det.Ssid = "RID-" + droneSerial
		det.OperatorId = "USA-SIM-001"
	} else {
		// === RemoteID OFF: WiFi beacon only ===
		// Only MAC, RSSI, channel - NO GPS, NO serial, NO operator
		det.Identifier = droneMAC // TAP uses MAC when no RemoteID
		det.Manufacturer = ""
		det.Designation = ""
		det.Source = pb.DetectionSource_SOURCE_WIFI_BEACON
		det.Ssid = droneSSID // DJI SSID pattern still visible
	}

	return det
}

// ============================================================
// Main
// ============================================================

func main() {
	fmt.Println("=============================================================")
	fmt.Println("  SKYLENS FLIGHT SIMULATOR - DJI Air 2S Lifecycle Test")
	fmt.Println("=============================================================")
	fmt.Println()
	fmt.Println("Drone:    DJI Air 2S")
	fmt.Printf("Serial:   %s\n", droneSerial)
	fmt.Printf("MAC:      %s\n", droneMAC)
	fmt.Println()
	fmt.Println("TAPs:")
	for _, t := range taps {
		fmt.Printf("  %s: %.6f, %.6f\n", t.ID, t.Lat, t.Lon)
	}
	fmt.Println()
	fmt.Println("Flight plan:")
	fmt.Println("  Phase 1 (0-45s):    APPROACH from 2km NE     [RemoteID ON]")
	fmt.Println("  Phase 2 (45-120s):  CIRCLE at 400m radius    [RemoteID ON]  <- calibration")
	fmt.Println("  Phase 3 (120-180s): DARK CIRCLE               [RemoteID OFF] <- identity recall")
	fmt.Println("  Phase 4 (180-240s): DARK DEPART to 2km SW     [RemoteID OFF] <- signal fading")
	fmt.Println()
	fmt.Println("Watch the dashboard for:")
	fmt.Println("  - GPS marker during phases 1-2")
	fmt.Println("  - Range rings from all 3 TAPs")
	fmt.Println("  - Model recalled as 'Air 2S' when RemoteID goes OFF")
	fmt.Println("  - Range rings CONTINUE with calibrated RSSI in phases 3-4")
	fmt.Println("  - Trilateration position estimate from 3 TAPs")
	fmt.Println()

	// Connect to NATS
	nc, err := nats.Connect("nats://localhost:4222", nats.Name("flight-sim"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to NATS: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()
	fmt.Println("Connected to NATS")
	fmt.Println()

	// Seed random for RSSI noise
	rand.Seed(time.Now().UnixNano())

	startTime := time.Now()
	totalDuration := 240.0 // seconds
	step := 0
	lastPhase := phase(-1)

	for {
		elapsed := time.Since(startTime).Seconds()
		if elapsed > totalDuration {
			break
		}

		currentPhase := getPhase(elapsed)
		lat, lon, heading, speed := dronePosition(elapsed)

		// Print phase transitions
		if currentPhase != lastPhase {
			fmt.Println()
			fmt.Printf("========== %s ==========\n", currentPhase)
			if currentPhase == phaseDarkCircle {
				fmt.Println(">>> REMOTEID KILLED - Switching to WiFi-only detection")
				fmt.Println(">>> System should recall model via identity memory")
				fmt.Println(">>> Range rings should use LIVE CALIBRATED RSSI_0")
			}
			if currentPhase == phaseDarkDepart {
				fmt.Println(">>> Drone departing - RSSI weakening across all TAPs")
			}
			lastPhase = currentPhase
		}

		// Send detection from each TAP
		for i, tap := range taps {
			det := buildDetection(tap, elapsed)
			data, err := proto.Marshal(det)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Marshal error: %v\n", err)
				continue
			}

			subject := fmt.Sprintf("skylens.detections.%s", tap.ID)
			if err := nc.Publish(subject, data); err != nil {
				fmt.Fprintf(os.Stderr, "Publish error: %v\n", err)
				continue
			}

			// Small delay between TAP detections (realistic)
			if i < len(taps)-1 {
				time.Sleep(tapDelay)
			}
		}
		nc.Flush()

		// Log status
		dist001 := haversineM(lat, lon, taps[0].Lat, taps[0].Lon)
		dist002 := haversineM(lat, lon, taps[1].Lat, taps[1].Lon)

		if currentPhase.hasRemoteID() {
			fmt.Printf("[%3.0fs] GPS: %.5f, %.5f | hdg: %3.0f | spd: %.0f m/s | d(tap1): %5.0fm d(tap2): %5.0fm | RSSI~%d/%d/%d\n",
				elapsed, lat, lon, heading, speed,
				dist001, dist002,
				simulateRSSI(dist001), simulateRSSI(dist002), simulateRSSI(dist002),
			)
		} else {
			fmt.Printf("[%3.0fs] WiFi-only | d(tap1): %5.0fm d(tap2): %5.0fm | RSSI~%d/%d/%d\n",
				elapsed,
				dist001, dist002,
				simulateRSSI(dist001), simulateRSSI(dist002), simulateRSSI(dist002),
			)
		}

		step++
		time.Sleep(updateInterval)
	}

	fmt.Println()
	fmt.Println("=============================================================")
	fmt.Println("  FLIGHT COMPLETE")
	fmt.Println("=============================================================")
	fmt.Println()
	fmt.Printf("Total detections sent: %d (across %d TAPs)\n", step*len(taps), len(taps))
	fmt.Println()
	fmt.Println("Check the dashboard:")
	fmt.Println("  1. /api/system/stats -> rssi_calibration should show Air 2S data")
	fmt.Println("  2. Drone should still show model='Air 2S' even after RemoteID OFF")
	fmt.Println("  3. Range rings should have source='live_tap' (not 'generic')")
	fmt.Println()

	// Print calibration stats
	fmt.Println("To verify calibration:")
	fmt.Println("  curl -s -b /tmp/cookies.txt http://localhost:8080/api/system/stats | python3 -m json.tool | grep -A 30 rssi_calibration")
}
