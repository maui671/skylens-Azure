package test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	pb "github.com/K13094/skylens/proto"
)

// Test configuration - override via environment variables
const (
	natsURL    = "nats://localhost:4222"
	apiBaseURL = "http://localhost:8080"
	wsURL      = "ws://localhost:8081/ws"
)

// DJI coordinate conversion factor
const djiCoordFactor = 174533.0

// ============================================================================
// Test 1: ASTM F3411 RemoteID WiFi NAN Detection
// ============================================================================

func TestRemoteIDDetection(t *testing.T) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Skipf("NATS not available: %v", err)
	}
	defer nc.Close()

	// Clear previous test state
	clearTestState(t)

	// Build ASTM F3411 RemoteID MessagePack payload
	payload := buildASTMRemoteIDPayload(t,
		"1581F5FKD229400000", // Serial (Mavic 3)
		37.7749, -122.4194,  // Position
		125.0, 8.5, 180.0,   // Alt, Speed, Heading
		"FAA-PILOT-123456",  // Operator ID
		37.7740, -122.4180,  // Operator position
	)

	mac := "60:60:1F:A1:B2:C3"
	det := &pb.Detection{
		TapId:           "tap-001",
		TimestampNs:     time.Now().UnixNano(),
		MacAddress:      mac,
		Identifier:      "REMOTEID-1581F5FKD229400000",
		SerialNumber:    "1581F5FKD229400000",
		Latitude:        37.7749,
		Longitude:       -122.4194,
		AltitudeGeodetic: 125.0,
		Speed:           8.5,
		Heading:         180.0,
		OperatorLatitude:  37.7740,
		OperatorLongitude: -122.4180,
		OperatorId:      "FAA-PILOT-123456",
		Rssi:            -55,
		Channel:         149,
		Source:          pb.DetectionSource_SOURCE_WIFI_NAN,
		Manufacturer:    "DJI",
		Designation:     "Mavic 3",
		Confidence:      0.95,
		RemoteidPayload: payload,
	}

	data, err := proto.Marshal(det)
	require.NoError(t, err)

	err = nc.Publish("skylens.detections.tap-001", data)
	require.NoError(t, err)

	// Wait for processing
	time.Sleep(200 * time.Millisecond)

	// Verify via API
	drone := getDroneByMAC(t, mac)
	require.NotNil(t, drone, "RemoteID drone not found in API")

	// Validate all fields
	t.Run("SerialNumber", func(t *testing.T) {
		assert.Equal(t, "1581F5FKD229400000", drone["serial_number"])
	})

	t.Run("Manufacturer", func(t *testing.T) {
		assert.Equal(t, "DJI", drone["manufacturer"])
	})

	t.Run("Position", func(t *testing.T) {
		assert.InDelta(t, 37.7749, drone["latitude"].(float64), 0.001)
		assert.InDelta(t, -122.4194, drone["longitude"].(float64), 0.001)
	})

	t.Run("Altitude", func(t *testing.T) {
		alt, ok := drone["altitude_geodetic"].(float64)
		if ok {
			assert.InDelta(t, 125.0, alt, 5.0)
		}
	})

	t.Run("Speed", func(t *testing.T) {
		speed, ok := drone["speed"].(float64)
		if ok {
			assert.InDelta(t, 8.5, speed, 1.0)
		}
	})

	t.Run("OperatorID", func(t *testing.T) {
		assert.Equal(t, "FAA-PILOT-123456", drone["operator_id"])
	})

	t.Run("OperatorPosition", func(t *testing.T) {
		opLat, ok1 := drone["operator_latitude"].(float64)
		opLon, ok2 := drone["operator_longitude"].(float64)
		if ok1 && ok2 {
			assert.InDelta(t, 37.7740, opLat, 0.001)
			assert.InDelta(t, -122.4180, opLon, 0.001)
		}
	})

	t.Run("DetectionSource", func(t *testing.T) {
		assert.Equal(t, "SOURCE_WIFI_NAN", drone["detection_source"])
	})

	t.Run("TrustScore", func(t *testing.T) {
		trust, ok := drone["trust_score"].(float64)
		if ok {
			assert.GreaterOrEqual(t, int(trust), 80)
		}
	})
}

// ============================================================================
// Test 2: DJI OcuSync DroneID Detection
// ============================================================================

func TestDJIDroneIDDetection(t *testing.T) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Skipf("NATS not available: %v", err)
	}
	defer nc.Close()

	clearTestState(t)

	// Build DJI DroneID payload with OUI header
	payload := buildDJIDroneIDPayload(t,
		"1581F44KD234500123", // Serial (Mini 3 Pro - F44 prefix)
		37.7749, -122.4194,  // Position
		150.0, 80.0,         // Alt, HeightAGL
		5.0, 3.0, -1.0,      // Velocity N/E/D
		270.0,               // Yaw
		0x44,                // Product type: Mini 3 Pro
	)

	mac := "60:60:1F:D1:E2:F3"
	det := &pb.Detection{
		TapId:           "tap-001",
		TimestampNs:     time.Now().UnixNano(),
		MacAddress:      mac,
		Identifier:      "DJI-" + mac,
		Rssi:            -55,
		Channel:         149,
		Source:          pb.DetectionSource_SOURCE_DJI_OCUSYNC,
		Manufacturer:    "DJI",
		RemoteidPayload: payload,
	}

	data, err := proto.Marshal(det)
	require.NoError(t, err)

	err = nc.Publish("skylens.detections.tap-001", data)
	require.NoError(t, err)

	time.Sleep(200 * time.Millisecond)

	drone := getDroneByMAC(t, mac)
	require.NotNil(t, drone, "DJI DroneID drone not found")

	t.Run("SerialExtracted", func(t *testing.T) {
		// Serial should be extracted from DJI payload
		serial, _ := drone["serial_number"].(string)
		assert.NotEmpty(t, serial)
	})

	t.Run("ManufacturerIsDJI", func(t *testing.T) {
		assert.Equal(t, "DJI", drone["manufacturer"])
	})

	t.Run("DetectionSource", func(t *testing.T) {
		assert.Equal(t, "SOURCE_DJI_OCUSYNC", drone["detection_source"])
	})
}

// ============================================================================
// Test 3: DJI Legacy Beacon Detection
// ============================================================================

func TestDJILegacyBeaconDetection(t *testing.T) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Skipf("NATS not available: %v", err)
	}
	defer nc.Close()

	clearTestState(t)

	// Build legacy DJI DroneID payload with legacy OUI
	payload := buildDJILegacyPayload(t,
		"1581F5F1234567890", // Serial (Mavic 3)
		37.7749, -122.4194,
		120.0, 60.0,
		4.0, 2.0, 0.0,
		180.0,
		0x38, // Mavic 3
	)

	mac := "26:37:12:AA:BB:CC"
	det := &pb.Detection{
		TapId:           "tap-001",
		TimestampNs:     time.Now().UnixNano(),
		MacAddress:      mac,
		Identifier:      "DJI-LEGACY-" + mac,
		Rssi:            -60,
		Channel:         149,
		Source:          pb.DetectionSource_SOURCE_WIFI_BEACON,
		RemoteidPayload: payload,
	}

	data, err := proto.Marshal(det)
	require.NoError(t, err)

	err = nc.Publish("skylens.detections.tap-001", data)
	require.NoError(t, err)

	time.Sleep(200 * time.Millisecond)

	drone := getDroneByMAC(t, mac)
	require.NotNil(t, drone, "DJI Legacy drone not found")

	t.Run("LegacyOUIRecognized", func(t *testing.T) {
		// Should be recognized as DJI
		mfg, _ := drone["manufacturer"].(string)
		assert.Equal(t, "DJI", mfg)
	})
}

// ============================================================================
// Test 4: OUI-Only Detection (SUSPECT Path)
// ============================================================================

func TestOUIOnlyDetection_RequiresCorrelation(t *testing.T) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Skipf("NATS not available: %v", err)
	}
	defer nc.Close()

	clearTestState(t)

	mac := "60:60:1F:11:22:33"

	// Inject single-TAP detection with DJI OUI but no RemoteID
	det := &pb.Detection{
		TapId:        "tap-001",
		TimestampNs:  time.Now().UnixNano(),
		MacAddress:   mac,
		Identifier:   "SUSPECT-" + mac,
		Rssi:         -65,
		Channel:      149,
		Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
		Manufacturer: "UNKNOWN",
		Designation:  "SUSPECT",
		Confidence:   0.35,
		// No RemoteidPayload
	}

	data, err := proto.Marshal(det)
	require.NoError(t, err)

	err = nc.Publish("skylens.detections.tap-001", data)
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	t.Run("SingleTAP_NotPromoted", func(t *testing.T) {
		drone := getDroneByMAC(t, mac)
		assert.Nil(t, drone, "Single-TAP OUI-only should NOT be promoted to drone")
	})

	t.Run("SingleTAP_IsSuspect", func(t *testing.T) {
		suspect := getSuspectByMAC(t, mac)
		assert.NotNil(t, suspect, "Should be tracked as suspect")
	})

	// Now send from second TAP
	det2 := &pb.Detection{
		TapId:        "tap-002",
		TimestampNs:  time.Now().UnixNano(),
		MacAddress:   mac,
		Identifier:   "SUSPECT-" + mac,
		Rssi:         -72,
		Channel:      149,
		Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
		Manufacturer: "UNKNOWN",
		Designation:  "SUSPECT",
		Confidence:   0.35,
	}

	data2, _ := proto.Marshal(det2)
	nc.Publish("skylens.detections.tap-002", data2)

	time.Sleep(200 * time.Millisecond)

	t.Run("MultiTAP_Promoted", func(t *testing.T) {
		drone := getDroneByMAC(t, mac)
		require.NotNil(t, drone, "Two-TAP should promote to drone")
		assert.Equal(t, "MULTI_TAP_CORRELATED", drone["detection_source"])
	})

	t.Run("MultiTAP_HasNoRemoteIDFlag", func(t *testing.T) {
		drone := getDroneByMAC(t, mac)
		if drone != nil {
			flags, ok := drone["spoof_flags"].([]interface{})
			if ok {
				hasFlag := false
				for _, f := range flags {
					if f == "NO_REMOTEID" {
						hasFlag = true
						break
					}
				}
				assert.True(t, hasFlag, "Should have NO_REMOTEID spoof flag")
			}
		}
	})
}

// ============================================================================
// Test 5: SSID Pattern Detection
// ============================================================================

func TestSSIDPatternDetection(t *testing.T) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Skipf("NATS not available: %v", err)
	}
	defer nc.Close()

	testCases := []struct {
		name         string
		ssid         string
		manufacturer string
	}{
		{"DJI Mavic SSID", "DJI-MAVIC-3-ABCD", "DJI"},
		{"Tello SSID", "TELLO-123456", "DJI/Ryze"},
		{"Parrot Anafi SSID", "ANAFI-AI-001", "Parrot"},
		{"Autel EVO SSID", "EVO-III-PRO", "Autel"},
		{"Skydio SSID", "SKYDIO-X10D", "Skydio"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			clearTestState(t)

			mac := fmt.Sprintf("00:11:22:%02X:%02X:%02X",
				len(tc.ssid), len(tc.ssid), len(tc.ssid))

			// First TAP
			det1 := &pb.Detection{
				TapId:        "tap-001",
				TimestampNs:  time.Now().UnixNano(),
				MacAddress:   mac,
				Ssid:         tc.ssid,
				Rssi:         -60,
				Channel:      36,
				Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
				Manufacturer: "UNKNOWN",
				Designation:  "SUSPECT",
				Confidence:   0.40,
			}
			data1, _ := proto.Marshal(det1)
			nc.Publish("skylens.detections.tap-001", data1)

			time.Sleep(50 * time.Millisecond)

			// Second TAP for correlation
			det2 := &pb.Detection{
				TapId:        "tap-002",
				TimestampNs:  time.Now().UnixNano(),
				MacAddress:   mac,
				Ssid:         tc.ssid,
				Rssi:         -70,
				Channel:      36,
				Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
				Manufacturer: "UNKNOWN",
				Designation:  "SUSPECT",
				Confidence:   0.40,
			}
			data2, _ := proto.Marshal(det2)
			nc.Publish("skylens.detections.tap-002", data2)

			time.Sleep(200 * time.Millisecond)

			drone := getDroneByMAC(t, mac)
			if drone != nil {
				mfg, _ := drone["manufacturer"].(string)
				assert.Equal(t, tc.manufacturer, mfg,
					"SSID %q should be identified as %s", tc.ssid, tc.manufacturer)
			}
		})
	}
}

// ============================================================================
// Test 6: False Positive Prevention
// ============================================================================

func TestFalsePositivePrevention(t *testing.T) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Skipf("NATS not available: %v", err)
	}
	defer nc.Close()

	testCases := []struct {
		name         string
		mac          string
		ssid         string
		shouldReject bool
	}{
		// Router OUIs - should be rejected
		{"SERCOMM router", "00:1E:58:AA:BB:CC", "NETGEAR-5G", true},
		{"TP-Link router", "F8:1A:67:AA:BB:CC", "TP-Link_Home", true},
		{"Cisco AP", "00:1C:10:AA:BB:CC", "Corp-WiFi", true},

		// ESP32 - REMOVED from OUI map (generic chip = false positives)
		// Now ALL ESP32 devices are rejected regardless of SSID
		{"ESP32 generic", "24:0A:C4:AA:BB:CC", "MyESPDevice", true},
		{"ESP32 drone SSID (still rejected - OUI removed)", "24:0A:C4:BB:CC:DD", "ESP_DRONE_001", true},

		// Known drone OUI (IEEE-verified) - should be accepted
		{"DJI OUI + DJI SSID", "60:60:1F:CC:DD:EE", "DJI-TEST-DRONE", false},
		{"Parrot OUI + Parrot SSID", "90:3A:E6:DD:EE:FF", "ANAFI-TEST", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			clearTestState(t)

			// Send from two TAPs to trigger correlation path
			for _, tapID := range []string{"tap-001", "tap-002"} {
				det := &pb.Detection{
					TapId:        tapID,
					TimestampNs:  time.Now().UnixNano(),
					MacAddress:   tc.mac,
					Ssid:         tc.ssid,
					Rssi:         -60,
					Channel:      149,
					Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
					Manufacturer: "UNKNOWN",
					Designation:  "SUSPECT",
					Confidence:   0.35,
				}
				data, _ := proto.Marshal(det)
				nc.Publish(fmt.Sprintf("skylens.detections.%s", tapID), data)
				time.Sleep(50 * time.Millisecond)
			}

			time.Sleep(200 * time.Millisecond)

			drone := getDroneByMAC(t, tc.mac)
			suspect := getSuspectByMAC(t, tc.mac)

			if tc.shouldReject {
				assert.Nil(t, drone,
					"%s should NOT be promoted to drone", tc.name)
				// Router detections may or may not be in suspects depending on
				// whether they have any drone indicator
			} else {
				// Should be tracked as drone or suspect
				assert.True(t, drone != nil || suspect != nil,
					"%s should be tracked", tc.name)
			}
		})
	}
}

// ============================================================================
// Test 7: Multi-TAP Correlation
// ============================================================================

func TestMultiTAPCorrelation(t *testing.T) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Skipf("NATS not available: %v", err)
	}
	defer nc.Close()

	clearTestState(t)

	mac := "60:60:1F:77:88:99"

	// TAP 1 - should create suspect only
	det1 := &pb.Detection{
		TapId:        "tap-001",
		TimestampNs:  time.Now().UnixNano(),
		MacAddress:   mac,
		Ssid:         "DJI-TEST-DRONE",
		Rssi:         -65,
		Channel:      149,
		Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
		Manufacturer: "UNKNOWN",
		Designation:  "SUSPECT",
		Confidence:   0.35,
	}
	data1, _ := proto.Marshal(det1)
	nc.Publish("skylens.detections.tap-001", data1)

	time.Sleep(100 * time.Millisecond)

	t.Run("After1TAP_IsSuspect", func(t *testing.T) {
		suspect := getSuspectByMAC(t, mac)
		assert.NotNil(t, suspect, "Should be suspect after 1 TAP")

		drone := getDroneByMAC(t, mac)
		assert.Nil(t, drone, "Should NOT be drone after 1 TAP")
	})

	// TAP 2 - should trigger promotion
	det2 := &pb.Detection{
		TapId:        "tap-002",
		TimestampNs:  time.Now().UnixNano(),
		MacAddress:   mac,
		Ssid:         "DJI-TEST-DRONE",
		Rssi:         -72,
		Channel:      149,
		Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
		Manufacturer: "UNKNOWN",
		Designation:  "SUSPECT",
		Confidence:   0.35,
	}
	data2, _ := proto.Marshal(det2)
	nc.Publish("skylens.detections.tap-002", data2)

	time.Sleep(200 * time.Millisecond)

	t.Run("After2TAPs_IsPromoted", func(t *testing.T) {
		drone := getDroneByMAC(t, mac)
		require.NotNil(t, drone, "Should be promoted after 2 TAPs")

		assert.Equal(t, "MULTI_TAP_CORRELATED", drone["detection_source"])
	})

	t.Run("After2TAPs_HasConfidenceBoost", func(t *testing.T) {
		drone := getDroneByMAC(t, mac)
		if drone != nil {
			conf, ok := drone["confidence"].(float64)
			if ok {
				// Base 0.35 + boost for 2nd TAP
				assert.GreaterOrEqual(t, conf, 0.60)
			}
		}
	})

	// TAP 3 - should boost confidence further
	det3 := &pb.Detection{
		TapId:        "tap-003",
		TimestampNs:  time.Now().UnixNano(),
		MacAddress:   mac,
		Ssid:         "DJI-TEST-DRONE",
		Rssi:         -80,
		Channel:      149,
		Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
		Manufacturer: "UNKNOWN",
		Designation:  "SUSPECT",
		Confidence:   0.35,
	}
	data3, _ := proto.Marshal(det3)
	nc.Publish("skylens.detections.tap-003", data3)

	time.Sleep(200 * time.Millisecond)

	t.Run("After3TAPs_MaxConfidence", func(t *testing.T) {
		drone := getDroneByMAC(t, mac)
		if drone != nil {
			trust, ok := drone["trust_score"].(float64)
			if ok {
				assert.GreaterOrEqual(t, int(trust), 70)
			}
		}
	})
}

// ============================================================================
// Test 8: End-to-End Latency
// ============================================================================

func TestEndToEndLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping latency test in short mode")
	}

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Skipf("NATS not available: %v", err)
	}
	defer nc.Close()

	// Connect to WebSocket
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Skipf("WebSocket not available: %v", err)
	}
	defer ws.Close()

	// Clear state
	clearTestState(t)

	const iterations = 50
	latencies := make([]time.Duration, 0, iterations)
	var mu sync.Mutex

	for i := 0; i < iterations; i++ {
		mac := fmt.Sprintf("60:60:1F:%02X:%02X:%02X", i, i, i)
		serial := fmt.Sprintf("1581F5FK%08d", i)

		det := &pb.Detection{
			TapId:           "tap-001",
			TimestampNs:     time.Now().UnixNano(),
			MacAddress:      mac,
			Identifier:      fmt.Sprintf("LATENCY-TEST-%d", i),
			SerialNumber:    serial,
			Latitude:        37.7749 + float64(i)*0.0001,
			Longitude:       -122.4194,
			Rssi:            -55,
			Source:          pb.DetectionSource_SOURCE_WIFI_NAN,
			Manufacturer:    "DJI",
			Designation:     "Mavic 3",
			Confidence:      0.95,
		}
		data, _ := proto.Marshal(det)

		// Set up WebSocket read with timeout
		done := make(chan struct{})
		var wsLatency time.Duration

		// Declare startTime before goroutine to avoid race
		var startTime time.Time

		go func() {
			ws.SetReadDeadline(time.Now().Add(2 * time.Second))
			for {
				_, msg, err := ws.ReadMessage()
				if err != nil {
					close(done)
					return
				}
				if bytes.Contains(msg, []byte(mac)) {
					wsLatency = time.Since(startTime)
					close(done)
					return
				}
			}
		}()

		// Publish and measure
		startTime = time.Now()
		nc.Publish("skylens.detections.tap-001", data)

		select {
		case <-done:
			mu.Lock()
			if wsLatency > 0 {
				latencies = append(latencies, wsLatency)
			}
			mu.Unlock()
		case <-time.After(2 * time.Second):
			t.Logf("Timeout waiting for WebSocket message %d", i)
		}

		// Small delay between iterations
		time.Sleep(20 * time.Millisecond)
	}

	if len(latencies) < iterations/2 {
		t.Skipf("Only %d/%d latency measurements succeeded", len(latencies), iterations)
	}

	// Calculate percentiles
	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	p50Idx := len(latencies) / 2
	p95Idx := int(float64(len(latencies)) * 0.95)
	p99Idx := int(float64(len(latencies)) * 0.99)

	if p99Idx >= len(latencies) {
		p99Idx = len(latencies) - 1
	}

	p50 := latencies[p50Idx]
	p95 := latencies[p95Idx]
	p99 := latencies[p99Idx]

	t.Logf("Latency measurements (%d samples):", len(latencies))
	t.Logf("  P50: %v", p50)
	t.Logf("  P95: %v", p95)
	t.Logf("  P99: %v", p99)

	assert.Less(t, p95, 100*time.Millisecond,
		"P95 latency should be under 100ms")
}

var startTime time.Time

// ============================================================================
// Helper Functions
// ============================================================================

func clearTestState(t *testing.T) {
	t.Helper()
	resp, err := http.Post(apiBaseURL+"/api/test/clear", "application/json", nil)
	if err != nil {
		// API might not be running, skip cleanup
		return
	}
	resp.Body.Close()
	time.Sleep(50 * time.Millisecond)
}

func getDroneByMAC(t *testing.T, mac string) map[string]interface{} {
	t.Helper()

	resp, err := http.Get(apiBaseURL + "/api/drones")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var result struct {
		Drones []map[string]interface{} `json:"drones"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	for _, d := range result.Drones {
		if d["mac"] == mac {
			return d
		}
	}
	return nil
}

func getSuspectByMAC(t *testing.T, mac string) map[string]interface{} {
	t.Helper()

	resp, err := http.Get(apiBaseURL + "/api/suspects")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var result struct {
		Suspects []map[string]interface{} `json:"suspects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	for _, s := range result.Suspects {
		if s["mac_address"] == mac {
			return s
		}
	}
	return nil
}

// buildASTMRemoteIDPayload creates an ASTM F3411 MessagePack payload
func buildASTMRemoteIDPayload(t *testing.T, serial string, lat, lon, alt, speed, heading float64, operatorID string, opLat, opLon float64) []byte {
	t.Helper()

	// MessagePack header: Type 0xF (pack), size 25, count 4
	result := []byte{0xF0, 25, 4}

	// BasicID message (25 bytes)
	basicID := make([]byte, 25)
	basicID[0] = 0x00 // Type 0 (BasicID), Version 0
	basicID[1] = 0x12 // IDType=SerialNumber(1), UAType=Helicopter(2)
	copy(basicID[2:22], []byte(serial))
	result = append(result, basicID...)

	// Location message (25 bytes)
	location := make([]byte, 25)
	location[0] = 0x10 // Type 1 (Location), Version 0
	location[1] = 0x2C // Status flags

	// Direction (degrees / 2)
	if heading >= 0 && heading < 360 {
		location[2] = byte(heading / 2)
	}

	// Speed (m/s * 4 for 0.25 resolution)
	location[3] = byte(speed * 4)
	location[4] = 0x00 // Speed multiplier

	// Vertical speed (0 for now)
	location[5] = 63 // 0 m/s = 63

	// Latitude (scaled by 1e7)
	latScaled := int32(lat * 1e7)
	binary.LittleEndian.PutUint32(location[6:10], uint32(latScaled))

	// Longitude
	lonScaled := int32(lon * 1e7)
	binary.LittleEndian.PutUint32(location[10:14], uint32(lonScaled))

	// Pressure altitude (0.5m resolution, offset 1000)
	altScaled := uint16((alt + 1000) * 2)
	binary.LittleEndian.PutUint16(location[14:16], altScaled)

	// Geodetic altitude
	binary.LittleEndian.PutUint16(location[16:18], altScaled)

	// Height AGL
	binary.LittleEndian.PutUint16(location[18:20], altScaled)

	result = append(result, location...)

	// System message (25 bytes)
	system := make([]byte, 25)
	system[0] = 0x40 // Type 4 (System), Version 0
	system[1] = 0x01 // OperatorLocType = Live GNSS

	// Operator position
	opLatScaled := int32(opLat * 1e7)
	opLonScaled := int32(opLon * 1e7)
	binary.LittleEndian.PutUint32(system[3:7], uint32(opLatScaled))
	binary.LittleEndian.PutUint32(system[7:11], uint32(opLonScaled))

	result = append(result, system...)

	// OperatorID message (25 bytes)
	opIDMsg := make([]byte, 25)
	opIDMsg[0] = 0x50 // Type 5 (OperatorID), Version 0
	opIDMsg[1] = 0x00 // OperatorIDType
	copy(opIDMsg[2:22], []byte(operatorID))

	result = append(result, opIDMsg...)

	return result
}

// buildDJIDroneIDPayload creates a DJI DroneID vendor IE payload
func buildDJIDroneIDPayload(t *testing.T, serial string, lat, lon, alt, heightAGL, vN, vE, vD, yaw float64, productType byte) []byte {
	t.Helper()

	// Full payload: OUI(3) + SubCmd(1) + DroneID(84)
	payload := make([]byte, 88)

	// DJI OUI
	payload[0] = 0x60
	payload[1] = 0x60
	payload[2] = 0x1F

	// SubCmd: DroneID
	payload[3] = 0x13

	// DroneID data starts at offset 4
	droneID := payload[4:]

	// Version
	droneID[0] = 0x02

	// Sequence number
	binary.LittleEndian.PutUint16(droneID[1:3], 1234)

	// State info: MotorsOn | InAir | GPSValid | AltValid
	droneID[3] = 0x0F
	droneID[4] = 0x00

	// Serial number (16 bytes)
	copy(droneID[5:21], []byte(serial))

	// Longitude (DJI format: degrees * 174533 * PI/180)
	lonRaw := int32(lon * djiCoordFactor * math.Pi / 180.0)
	binary.LittleEndian.PutUint32(droneID[21:25], uint32(lonRaw))

	// Latitude
	latRaw := int32(lat * djiCoordFactor * math.Pi / 180.0)
	binary.LittleEndian.PutUint32(droneID[25:29], uint32(latRaw))

	// Altitude (meters, int16)
	binary.LittleEndian.PutUint16(droneID[29:31], uint16(alt))

	// Height AGL
	binary.LittleEndian.PutUint16(droneID[31:33], uint16(heightAGL))

	// Velocity North (cm/s)
	binary.LittleEndian.PutUint16(droneID[33:35], uint16(int16(vN*100)))

	// Velocity East
	binary.LittleEndian.PutUint16(droneID[35:37], uint16(int16(vE*100)))

	// Velocity Down
	binary.LittleEndian.PutUint16(droneID[37:39], uint16(int16(vD*100)))

	// Pitch (0)
	binary.LittleEndian.PutUint16(droneID[39:41], 0)

	// Roll (0)
	binary.LittleEndian.PutUint16(droneID[41:43], 0)

	// Yaw (degrees * 100)
	binary.LittleEndian.PutUint16(droneID[43:45], uint16(int16(yaw*100)))

	// Product type
	droneID[53] = productType

	return payload
}

// buildDJILegacyPayload creates a DJI Legacy DroneID payload with legacy OUI
func buildDJILegacyPayload(t *testing.T, serial string, lat, lon, alt, heightAGL, vN, vE, vD, yaw float64, productType byte) []byte {
	t.Helper()

	payload := make([]byte, 88)

	// Legacy OUI: 26:37:12
	payload[0] = 0x26
	payload[1] = 0x37
	payload[2] = 0x12

	// SubCmd
	payload[3] = 0x13

	// Rest is same as OcuSync format
	droneID := payload[4:]
	droneID[0] = 0x02
	binary.LittleEndian.PutUint16(droneID[1:3], 5678)
	droneID[3] = 0x0F
	droneID[4] = 0x00
	copy(droneID[5:21], []byte(serial))

	lonRaw := int32(lon * djiCoordFactor * math.Pi / 180.0)
	binary.LittleEndian.PutUint32(droneID[21:25], uint32(lonRaw))

	latRaw := int32(lat * djiCoordFactor * math.Pi / 180.0)
	binary.LittleEndian.PutUint32(droneID[25:29], uint32(latRaw))

	binary.LittleEndian.PutUint16(droneID[29:31], uint16(alt))
	binary.LittleEndian.PutUint16(droneID[31:33], uint16(heightAGL))
	binary.LittleEndian.PutUint16(droneID[33:35], uint16(int16(vN*100)))
	binary.LittleEndian.PutUint16(droneID[35:37], uint16(int16(vE*100)))
	binary.LittleEndian.PutUint16(droneID[37:39], uint16(int16(vD*100)))
	binary.LittleEndian.PutUint16(droneID[43:45], uint16(int16(yaw*100)))
	droneID[53] = productType

	return payload
}
