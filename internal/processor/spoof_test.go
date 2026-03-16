package processor

import (
	"testing"
	"time"

	"github.com/K13094/skylens/internal/intel"
)

// TestCoordinateJump tests detection of impossible GPS teleportation
// A drone cannot move 10km in 1 second (would require >10,000 m/s)
func TestCoordinateJump(t *testing.T) {
	tests := []struct {
		name           string
		firstLat       float64
		firstLng       float64
		secondLat      float64
		secondLng      float64
		deltaSeconds   float64
		expectFlag     bool
		expectedFlag   string
	}{
		{
			name:         "10km jump in 1 second - definite spoof",
			firstLat:     37.7749,
			firstLng:     -122.4194,
			secondLat:    37.8649, // ~10km north
			secondLng:    -122.4194,
			deltaSeconds: 1.0,
			expectFlag:   true,
			expectedFlag: "coordinate_jump",
		},
		{
			name:         "5km jump in 1 second - spoof",
			firstLat:     37.7749,
			firstLng:     -122.4194,
			secondLat:    37.8199, // ~5km north
			secondLng:    -122.4194,
			deltaSeconds: 1.0,
			expectFlag:   true,
			expectedFlag: "coordinate_jump",
		},
		{
			name:         "100m in 1 second - normal flight",
			firstLat:     37.7749,
			firstLng:     -122.4194,
			secondLat:    37.7758, // ~100m
			secondLng:    -122.4194,
			deltaSeconds: 1.0,
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "10km in 10 minutes - normal flight",
			firstLat:     37.7749,
			firstLng:     -122.4194,
			secondLat:    37.8649,
			secondLng:    -122.4194,
			deltaSeconds: 600.0,
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "1km jump in 5 seconds - borderline (200 m/s)",
			firstLat:     37.7749,
			firstLng:     -122.4194,
			secondLat:    37.7839, // ~1km
			secondLng:    -122.4194,
			deltaSeconds: 5.0,
			expectFlag:   true,
			expectedFlag: "coordinate_jump",
		},
		{
			name:         "500m in 10 seconds - normal (50 m/s)",
			firstLat:     37.7749,
			firstLng:     -122.4194,
			secondLat:    37.7794, // ~500m
			secondLng:    -122.4194,
			deltaSeconds: 10.0,
			expectFlag:   false,
			expectedFlag: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detector := NewSpoofDetector()

			// First detection establishes baseline
			d1 := &Drone{
				Identifier:   "test-drone-001",
				MACAddress:   "60:60:1F:AA:BB:CC",
				SerialNumber: "1581F5FKD229400001",
				Latitude:     tc.firstLat,
				Longitude:    tc.firstLng,
			}
			detector.Analyze(d1)

			// Simulate time passage
			detector.modifyTrack("test-drone-001", func(track *droneTrack) {
				track.lastTime = time.Now().Add(-time.Duration(tc.deltaSeconds) * time.Second)
			})

			// Second detection with jump
			d2 := &Drone{
				Identifier:   "test-drone-001",
				MACAddress:   "60:60:1F:AA:BB:CC",
				SerialNumber: "1581F5FKD229400001",
				Latitude:     tc.secondLat,
				Longitude:    tc.secondLng,
			}
			_, flags := detector.Analyze(d2)

			hasFlag := containsFlag(flags, tc.expectedFlag)
			if tc.expectFlag && !hasFlag {
				t.Errorf("expected flag %q but got flags: %v", tc.expectedFlag, flags)
			}
			if !tc.expectFlag && hasFlag {
				t.Errorf("did not expect flag %q but got flags: %v", tc.expectedFlag, flags)
			}
		})
	}
}

// TestSpeedViolation tests detection of physically impossible reported speeds
// maxDroneSpeedMS is 80 m/s (~180 mph)
func TestSpeedViolation(t *testing.T) {
	tests := []struct {
		name         string
		speed        float32
		expectFlag   bool
		expectedFlag string
	}{
		{
			name:         "100 m/s - exceeds max",
			speed:        100.0,
			expectFlag:   true,
			expectedFlag: "speed_violation",
		},
		{
			name:         "200 m/s - way over max",
			speed:        200.0,
			expectFlag:   true,
			expectedFlag: "speed_violation",
		},
		{
			name:         "350 m/s - near speed of sound",
			speed:        350.0,
			expectFlag:   true,
			expectedFlag: "speed_violation",
		},
		{
			name:         "80 m/s - at limit (no flag)",
			speed:        80.0,
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "79 m/s - just under limit",
			speed:        79.0,
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "81 m/s - just over limit",
			speed:        81.0,
			expectFlag:   true,
			expectedFlag: "speed_violation",
		},
		{
			name:         "15 m/s - normal cruise speed",
			speed:        15.0,
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "0 m/s - hovering",
			speed:        0.0,
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "50 m/s - fast but reasonable",
			speed:        50.0,
			expectFlag:   false,
			expectedFlag: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detector := NewSpoofDetector()

			drone := &Drone{
				Identifier:   "test-drone-speed",
				MACAddress:   "60:60:1F:AA:BB:CC",
				SerialNumber: "1581F5FKD229400001",
				Speed:        tc.speed,
				Latitude:     37.7749,
				Longitude:    -122.4194,
			}

			_, flags := detector.Analyze(drone)

			hasFlag := containsFlag(flags, tc.expectedFlag)
			if tc.expectFlag && !hasFlag {
				t.Errorf("expected flag %q for speed %.1f m/s but got: %v", tc.expectedFlag, tc.speed, flags)
			}
			if !tc.expectFlag && hasFlag {
				t.Errorf("did not expect flag %q for speed %.1f m/s but got: %v", tc.expectedFlag, tc.speed, flags)
			}
		})
	}
}

// TestOUISSIDMismatch tests detection of OUI-SSID vendor inconsistency
// NOTE: ESP32 OUIs were REMOVED from the fingerprint database to prevent false positives.
// Now ESP32 devices are treated as "unknown OUI" and don't trigger mismatch flags.
// Only mismatches between KNOWN drone OUIs are flagged (e.g., DJI OUI + Parrot SSID)
func TestOUISSIDMismatch(t *testing.T) {
	tests := []struct {
		name         string
		mac          string
		ssid         string
		expectFlag   bool
		expectedFlag string
	}{
		// ESP32 OUIs are now UNKNOWN - no mismatch detection possible
		// This is a trade-off to prevent false positives from IoT devices
		{
			name:         "ESP32 OUI claiming DJI (unknown OUI, no flag)",
			mac:          "24:0A:C4:12:34:56", // Espressif - REMOVED from OUI map
			ssid:         "DJI-MAVIC3PRO",
			expectFlag:   false, // Changed: unknown OUI doesn't trigger flag
			expectedFlag: "",
		},
		{
			name:         "ESP32 OUI claiming Parrot (unknown OUI, no flag)",
			mac:          "30:AE:A4:AA:BB:CC", // Espressif - REMOVED from OUI map
			ssid:         "ANAFI-12345",
			expectFlag:   false, // Changed: unknown OUI doesn't trigger flag
			expectedFlag: "",
		},
		{
			name:         "ESP32 OUI claiming Autel (unknown OUI, no flag)",
			mac:          "7C:DF:A1:00:11:22", // Espressif - REMOVED from OUI map
			ssid:         "Autel-EVO-III",
			expectFlag:   false, // Changed: unknown OUI doesn't trigger flag
			expectedFlag: "",
		},
		{
			name:         "ESP32 OUI claiming Skydio (unknown OUI, no flag)",
			mac:          "84:F3:EB:33:44:55", // Espressif - REMOVED from OUI map
			ssid:         "SKYDIO-X10D",
			expectFlag:   false, // Changed: unknown OUI doesn't trigger flag
			expectedFlag: "",
		},
		// Legitimate drone OUI + SSID combinations - no flag
		{
			name:         "DJI OUI with DJI SSID - legitimate",
			mac:          "60:60:1F:AA:BB:CC", // DJI OUI (IEEE verified)
			ssid:         "DJI-MAVIC3PRO",
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "Parrot OUI with Parrot SSID - legitimate",
			mac:          "90:3A:E6:11:22:33", // Parrot OUI (IEEE verified)
			ssid:         "ANAFI-ABC123",
			expectFlag:   false,
			expectedFlag: "",
		},
		// Cross-vendor mismatches - THESE STILL WORK
		{
			name:         "DJI OUI claiming Parrot - mismatch",
			mac:          "60:60:1F:AA:BB:CC", // DJI OUI
			ssid:         "ANAFI-SPOOF",
			expectFlag:   true,
			expectedFlag: "oui_ssid_mismatch",
		},
		{
			name:         "Parrot OUI claiming DJI - mismatch",
			mac:          "90:3A:E6:11:22:33", // Parrot OUI
			ssid:         "DJI-MAVIC-FAKE",
			expectFlag:   true,
			expectedFlag: "oui_ssid_mismatch",
		},
		// Unknown OUI - no flag
		{
			name:         "Unknown OUI - no flag",
			mac:          "00:11:22:33:44:55", // Unknown OUI
			ssid:         "DJI-MAVIC3",
			expectFlag:   false,
			expectedFlag: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detector := NewSpoofDetector()

			drone := &Drone{
				Identifier:   "test-drone-oui",
				MACAddress:   tc.mac,
				SSID:         tc.ssid,
				SerialNumber: "1581F5FKD229400001",
				Latitude:     37.7749,
				Longitude:    -122.4194,
			}

			_, flags := detector.Analyze(drone)

			hasExpectedFlag := containsFlag(flags, tc.expectedFlag)
			if tc.expectFlag && !hasExpectedFlag {
				t.Errorf("expected flag %q for MAC %s / SSID %s but got: %v",
					tc.expectedFlag, tc.mac, tc.ssid, flags)
			}
			if !tc.expectFlag && tc.expectedFlag != "" && hasExpectedFlag {
				t.Errorf("did not expect flag %q for MAC %s / SSID %s but got: %v",
					tc.expectedFlag, tc.mac, tc.ssid, flags)
			}
		})
	}
}

// TestImpossibleVendor tests detection of OUI-SSID mismatches for enterprise drones
// NOTE: ESP32 OUIs were REMOVED from the database to prevent IoT false positives.
// We can no longer detect ESP32 claiming to be enterprise drones.
// This test now verifies legitimate OUI+SSID combos don't trigger flags.
func TestImpossibleVendor(t *testing.T) {
	tests := []struct {
		name         string
		mac          string
		ssid         string
		expectFlag   bool
		expectedFlag string
	}{
		// ESP32 OUIs are now UNKNOWN - can't detect impossible vendor claims
		// Trade-off: prevent false positives from smart home devices
		{
			name:         "ESP32 claiming DJI (unknown OUI, no flag)",
			mac:          "24:0A:C4:12:34:56", // ESP32 - REMOVED from OUI map
			ssid:         "MATRICE-350RTK",
			expectFlag:   false, // Changed: unknown OUI
			expectedFlag: "",
		},
		{
			name:         "ESP32 claiming DJI Inspire (unknown OUI, no flag)",
			mac:          "A4:CF:12:11:22:33", // ESP32 - REMOVED from OUI map
			ssid:         "INSPIRE-3",
			expectFlag:   false, // Changed: unknown OUI
			expectedFlag: "",
		},
		{
			name:         "ESP32 claiming Skydio (unknown OUI, no flag)",
			mac:          "FC:F5:C4:AA:BB:CC", // ESP32 - REMOVED from OUI map
			ssid:         "SKYDIO-X10D-001",
			expectFlag:   false, // Changed: unknown OUI
			expectedFlag: "",
		},
		{
			name:         "ESP32 claiming Autel (unknown OUI, no flag)",
			mac:          "30:AE:A4:99:88:77", // ESP32 - REMOVED from OUI map
			ssid:         "Autel-Dragonfish",  // Use SSID that matches pattern
			expectFlag:   false,               // Changed: unknown OUI
			expectedFlag: "",
		},
		// Legitimate combinations - no flags
		{
			name:         "Legitimate Skydio OUI with Skydio SSID",
			mac:          "38:1D:14:AA:BB:CC", // Skydio OUI (IEEE verified)
			ssid:         "SKYDIO-X10D",
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "Legitimate DJI enterprise",
			mac:          "60:60:1F:12:34:56", // DJI OUI (IEEE verified)
			ssid:         "MATRICE-30T",
			expectFlag:   false,
			expectedFlag: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detector := NewSpoofDetector()

			drone := &Drone{
				Identifier:   "test-impossible-vendor",
				MACAddress:   tc.mac,
				SSID:         tc.ssid,
				SerialNumber: "1581F5FKD229400001",
			}

			_, flags := detector.Analyze(drone)

			hasFlag := containsFlag(flags, tc.expectedFlag)
			if tc.expectFlag && !hasFlag {
				t.Errorf("expected flag %q but got: %v", tc.expectedFlag, flags)
			}
			if !tc.expectFlag && tc.expectedFlag != "" && hasFlag {
				t.Errorf("did not expect flag %q but got: %v", tc.expectedFlag, flags)
			}
		})
	}
}

// TestRandomizedMAC tests detection of locally administered (randomized) MAC addresses
// LAA bit is second-least significant bit of first octet
func TestRandomizedMAC(t *testing.T) {
	tests := []struct {
		name         string
		mac          string
		expectFlag   bool
		expectedFlag string
	}{
		{
			name:         "x2:xx:xx - LAA set (randomized)",
			mac:          "02:11:22:33:44:55",
			expectFlag:   true,
			expectedFlag: "randomized_mac",
		},
		{
			name:         "x6:xx:xx - LAA set",
			mac:          "06:AA:BB:CC:DD:EE",
			expectFlag:   true,
			expectedFlag: "randomized_mac",
		},
		{
			name:         "xA:xx:xx - LAA set",
			mac:          "0A:00:11:22:33:44",
			expectFlag:   true,
			expectedFlag: "randomized_mac",
		},
		{
			name:         "xE:xx:xx - LAA set",
			mac:          "0E:FF:EE:DD:CC:BB",
			expectFlag:   true,
			expectedFlag: "randomized_mac",
		},
		{
			name:         "DA:xx:xx - LAA set (D=1101, bit1=0, but A has bit1=1)",
			mac:          "DA:12:34:56:78:9A",
			expectFlag:   true,
			expectedFlag: "randomized_mac",
		},
		{
			name:         "60:60:1F - DJI OUI (globally unique)",
			mac:          "60:60:1F:AA:BB:CC",
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "00:12:1C - Parrot OUI (globally unique)",
			mac:          "00:12:1C:11:22:33",
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "24:0A:C4 - Espressif OUI (globally unique)",
			mac:          "24:0A:C4:44:55:66",
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "38:1D:14 - Skydio OUI (globally unique)",
			mac:          "38:1D:14:77:88:99",
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "Randomized with dashes format",
			mac:          "02-AA-BB-CC-DD-EE",
			expectFlag:   true,
			expectedFlag: "randomized_mac",
		},
		{
			name:         "Empty MAC - no flag",
			mac:          "",
			expectFlag:   false,
			expectedFlag: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detector := NewSpoofDetector()

			drone := &Drone{
				Identifier:   "test-drone-mac",
				MACAddress:   tc.mac,
				SerialNumber: "1581F5FKD229400001",
				Latitude:     37.7749,
				Longitude:    -122.4194,
			}

			_, flags := detector.Analyze(drone)

			hasFlag := containsFlag(flags, tc.expectedFlag)
			if tc.expectFlag && !hasFlag {
				t.Errorf("expected flag %q for MAC %s but got: %v", tc.expectedFlag, tc.mac, flags)
			}
			if !tc.expectFlag && tc.expectedFlag != "" && hasFlag {
				t.Errorf("did not expect flag %q for MAC %s but got: %v", tc.expectedFlag, tc.mac, flags)
			}
		})
	}
}

// TestAltitudeSpike tests detection of impossible altitude changes
// maxAltitudeChange is 100 m/s
func TestAltitudeSpike(t *testing.T) {
	tests := []struct {
		name         string
		firstAlt     float32
		secondAlt    float32
		deltaSeconds float64
		expectFlag   bool
		expectedFlag string
	}{
		{
			name:         "500m spike in 1 second - impossible",
			firstAlt:     100.0,
			secondAlt:    600.0,
			deltaSeconds: 1.0,
			expectFlag:   true,
			expectedFlag: "altitude_spike",
		},
		{
			name:         "200m drop in 1 second - impossible",
			firstAlt:     500.0,
			secondAlt:    300.0,
			deltaSeconds: 1.0,
			expectFlag:   true,
			expectedFlag: "altitude_spike",
		},
		{
			name:         "1000m change in 5 seconds - impossible (200 m/s)",
			firstAlt:     100.0,
			secondAlt:    1100.0,
			deltaSeconds: 5.0,
			expectFlag:   true,
			expectedFlag: "altitude_spike",
		},
		{
			name:         "50m in 1 second - aggressive but possible",
			firstAlt:     100.0,
			secondAlt:    150.0,
			deltaSeconds: 1.0,
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "100m in 1 second - at limit",
			firstAlt:     100.0,
			secondAlt:    200.0,
			deltaSeconds: 1.0,
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "101m in 1 second - just over limit",
			firstAlt:     100.0,
			secondAlt:    201.0,
			deltaSeconds: 1.0,
			expectFlag:   true,
			expectedFlag: "altitude_spike",
		},
		{
			name:         "500m change in 10 seconds - normal (50 m/s)",
			firstAlt:     100.0,
			secondAlt:    600.0,
			deltaSeconds: 10.0,
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "10m normal altitude adjustment",
			firstAlt:     120.0,
			secondAlt:    130.0,
			deltaSeconds: 1.0,
			expectFlag:   false,
			expectedFlag: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detector := NewSpoofDetector()

			// First detection establishes baseline
			d1 := &Drone{
				Identifier:       "test-drone-alt",
				MACAddress:       "60:60:1F:AA:BB:CC",
				SerialNumber:     "1581F5FKD229400001",
				AltitudeGeodetic: tc.firstAlt,
				Latitude:         37.7749,
				Longitude:        -122.4194,
			}
			detector.Analyze(d1)

			// Simulate time passage
			detector.modifyTrack("test-drone-alt", func(track *droneTrack) {
				track.lastTime = time.Now().Add(-time.Duration(tc.deltaSeconds) * time.Second)
			})

			// Second detection with altitude change
			d2 := &Drone{
				Identifier:       "test-drone-alt",
				MACAddress:       "60:60:1F:AA:BB:CC",
				SerialNumber:     "1581F5FKD229400001",
				AltitudeGeodetic: tc.secondAlt,
				Latitude:         37.7749,
				Longitude:        -122.4194,
			}
			_, flags := detector.Analyze(d2)

			hasFlag := containsFlag(flags, tc.expectedFlag)
			if tc.expectFlag && !hasFlag {
				t.Errorf("expected flag %q for altitude change %.1f->%.1f in %.1fs but got: %v",
					tc.expectedFlag, tc.firstAlt, tc.secondAlt, tc.deltaSeconds, flags)
			}
			if !tc.expectFlag && tc.expectedFlag != "" && hasFlag {
				t.Errorf("did not expect flag %q for altitude change %.1f->%.1f in %.1fs but got: %v",
					tc.expectedFlag, tc.firstAlt, tc.secondAlt, tc.deltaSeconds, flags)
			}
		})
	}
}

// TestTrustRecovery tests that consecutive clean samples restore trust
// trustRecoveryClean is 10 samples, trustRecoveryAmount is 5 points
// Note: Flags persist for flagExpirySec (300s), so recovery is gradual
func TestTrustRecovery(t *testing.T) {
	tests := []struct {
		name              string
		initialFlags      int // number of samples with flags
		cleanSamples      int // consecutive clean samples after
		expectFullTrust   bool
		expectPartialRecv bool
		minTrustScore     int
	}{
		{
			name:              "5 clean samples after flag - partial recovery bonus",
			initialFlags:      1,
			cleanSamples:      5,
			expectFullTrust:   false,
			expectPartialRecv: true,
			minTrustScore:     80, // 100 - 20 (altitude_spike) + 5 (partial recovery)
		},
		{
			name:              "10 clean samples after flag - recovery bonus applied",
			initialFlags:      1,
			cleanSamples:      10,
			expectFullTrust:   false, // Flag still persists until 300s expiry
			expectPartialRecv: true,
			minTrustScore:     85, // 100 - 20 + 5 recovery bonus
		},
		{
			name:              "15 clean samples no prior flags - sustained trust",
			initialFlags:      0,
			cleanSamples:      15,
			expectFullTrust:   true,
			expectPartialRecv: true,
			minTrustScore:     100,
		},
		{
			name:              "3 clean samples - not enough for recovery bonus",
			initialFlags:      1,
			cleanSamples:      3,
			expectFullTrust:   false,
			expectPartialRecv: false,
			minTrustScore:     70, // Flag penalty still applies, no recovery bonus yet
		},
		{
			name:              "Multiple flags then 10 clean - gradual recovery",
			initialFlags:      3,
			cleanSamples:      10,
			expectFullTrust:   false, // Flags persist
			expectPartialRecv: true,
			minTrustScore:     60, // Multiple flags but with recovery bonus
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detector := NewSpoofDetector()

			// Apply initial flags by sending detections with issues
			for i := 0; i < tc.initialFlags; i++ {
				// First sample establishes track
				if i == 0 {
					d := &Drone{
						Identifier:       "test-drone-recovery",
						MACAddress:       "60:60:1F:AA:BB:CC",
						SerialNumber:     "1581F5FKD229400001",
						Latitude:         37.7749,
						Longitude:        -122.4194,
						AltitudeGeodetic: 100.0,
					}
					detector.Analyze(d)

					// Set last time in past for next sample
					detector.modifyTrack("test-drone-recovery", func(track *droneTrack) {
						track.lastTime = time.Now().Add(-2 * time.Second)
					})
				}

				// Trigger altitude_spike flag
				d := &Drone{
					Identifier:       "test-drone-recovery",
					MACAddress:       "60:60:1F:AA:BB:CC",
					SerialNumber:     "1581F5FKD229400001",
					Latitude:         37.7749,
					Longitude:        -122.4194,
					AltitudeGeodetic: 700.0 + float32(i*100), // 600m spike
				}
				detector.Analyze(d)

				// Reset time for next iteration
				detector.modifyTrack("test-drone-recovery", func(track *droneTrack) {
					track.lastTime = time.Now().Add(-2 * time.Second)
				})
			}

			// Send clean samples
			var lastTrust int
			for i := 0; i < tc.cleanSamples; i++ {
				// Reset time
				detector.modifyTrack("test-drone-recovery", func(track *droneTrack) {
					track.lastTime = time.Now().Add(-2 * time.Second)
				})

				d := &Drone{
					Identifier:       "test-drone-recovery",
					MACAddress:       "60:60:1F:AA:BB:CC",
					SerialNumber:     "1581F5FKD229400001",
					Latitude:         37.7749,
					Longitude:        -122.4194,
					AltitudeGeodetic: 100.0, // stable altitude
				}
				lastTrust, _ = detector.Analyze(d)
			}

			if lastTrust < tc.minTrustScore {
				t.Errorf("expected trust >= %d after %d clean samples, got %d",
					tc.minTrustScore, tc.cleanSamples, lastTrust)
			}

			if tc.expectFullTrust && lastTrust < 100 {
				t.Errorf("expected full trust (100) but got %d", lastTrust)
			}
		})
	}
}

// TestTrustRecoveryWithFlagExpiry tests trust recovery when flags naturally expire
// Note: The Analyze function cleans expired flags at the start but may add new flags
// based on the current detection data. This test verifies expiry behavior for
// flags that are not re-triggered.
func TestTrustRecoveryWithFlagExpiry(t *testing.T) {
	detector := NewSpoofDetector()

	// Create initial detection that triggers "no_serial" flag
	d1 := &Drone{
		Identifier: "test-expiry",
		MACAddress: "60:60:1F:AA:BB:CC",
		// No serial number - triggers "no_serial" flag
		Latitude:  37.7749,
		Longitude: -122.4194,
	}
	trust1, flags1 := detector.Analyze(d1)

	if !containsFlag(flags1, "no_serial") {
		t.Error("expected no_serial flag")
	}

	// Verify trust is penalized
	if trust1 >= 100 {
		t.Errorf("expected reduced trust due to flag, got %d", trust1)
	}

	// Simulate flag expiry by manipulating flag timestamp
	detector.modifyTrack("test-expiry", func(track *droneTrack) {
		for flag := range track.flags {
			track.flags[flag] = time.Now().Add(-6 * time.Minute) // Past 5-minute expiry
		}
	})

	// Detection WITH serial should now have full trust (old flag expired, no new flag)
	d2 := &Drone{
		Identifier:   "test-expiry",
		MACAddress:   "60:60:1F:AA:BB:CC",
		SerialNumber: "1581F5FKD229400001", // Now has serial
		Latitude:     37.7749,
		Longitude:    -122.4194,
	}

	trust2, flags2 := detector.Analyze(d2)

	if len(flags2) != 0 {
		t.Errorf("expected no flags after expiry with valid serial, got %v", flags2)
	}
	if trust2 != 100 {
		t.Errorf("expected full trust after flag expiry, got %d", trust2)
	}
}

// TestFlagPersistence tests that flags persist for flagExpirySec (300 seconds)
func TestFlagPersistence(t *testing.T) {
	detector := NewSpoofDetector()

	// Create detection with no serial
	d1 := &Drone{
		Identifier: "test-persist",
		MACAddress: "60:60:1F:AA:BB:CC",
		Latitude:   37.7749,
		Longitude:  -122.4194,
	}
	_, flags1 := detector.Analyze(d1)

	if !containsFlag(flags1, "no_serial") {
		t.Fatal("expected no_serial flag")
	}

	// Verify flag persists on subsequent detection (still no serial)
	_, flags2 := detector.Analyze(d1)
	if !containsFlag(flags2, "no_serial") {
		t.Error("flag should persist across detections")
	}

	// Simulate partial time passage (2 minutes - within expiry window)
	detector.modifyTrack("test-persist", func(track *droneTrack) {
		for flag := range track.flags {
			track.flags[flag] = time.Now().Add(-2 * time.Minute)
		}
	})

	// Flag should still exist after 2 minutes
	_, flags3 := detector.Analyze(d1)
	if !containsFlag(flags3, "no_serial") {
		t.Error("flag should persist for 5 minutes, not 2")
	}
}

// TestMissingSerial tests detection of missing serial numbers
func TestMissingSerial(t *testing.T) {
	tests := []struct {
		name         string
		serial       string
		expectFlag   bool
		expectedFlag string
	}{
		{
			name:         "Empty serial - flagged",
			serial:       "",
			expectFlag:   true,
			expectedFlag: "no_serial",
		},
		{
			name:         "Valid DJI serial",
			serial:       "1581F5FKD229400001",
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "Short serial - still valid",
			serial:       "ABC123",
			expectFlag:   false,
			expectedFlag: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detector := NewSpoofDetector()

			drone := &Drone{
				Identifier:   "test-serial",
				MACAddress:   "60:60:1F:AA:BB:CC",
				SerialNumber: tc.serial,
				Latitude:     37.7749,
				Longitude:    -122.4194,
			}

			_, flags := detector.Analyze(drone)

			hasFlag := containsFlag(flags, tc.expectedFlag)
			if tc.expectFlag && !hasFlag {
				t.Errorf("expected flag %q for serial %q but got: %v", tc.expectedFlag, tc.serial, flags)
			}
			if !tc.expectFlag && tc.expectedFlag != "" && hasFlag {
				t.Errorf("did not expect flag %q for serial %q but got: %v", tc.expectedFlag, tc.serial, flags)
			}
		})
	}
}

// TestInvalidCoordinates tests detection of out-of-range GPS coordinates
func TestInvalidCoordinates(t *testing.T) {
	tests := []struct {
		name         string
		lat          float64
		lng          float64
		expectFlag   bool
		expectedFlag string
	}{
		{
			name:         "Latitude > 90",
			lat:          91.0,
			lng:          -122.0,
			expectFlag:   true,
			expectedFlag: "invalid_coordinates",
		},
		{
			name:         "Latitude < -90",
			lat:          -91.0,
			lng:          -122.0,
			expectFlag:   true,
			expectedFlag: "invalid_coordinates",
		},
		{
			name:         "Longitude > 180",
			lat:          37.0,
			lng:          181.0,
			expectFlag:   true,
			expectedFlag: "invalid_coordinates",
		},
		{
			name:         "Longitude < -180",
			lat:          37.0,
			lng:          -181.0,
			expectFlag:   true,
			expectedFlag: "invalid_coordinates",
		},
		{
			name:         "Valid San Francisco",
			lat:          37.7749,
			lng:          -122.4194,
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "Valid North Pole",
			lat:          90.0,
			lng:          0.0,
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "Valid date line",
			lat:          0.0,
			lng:          180.0,
			expectFlag:   false,
			expectedFlag: "",
		},
		{
			name:         "Zero coordinates - no flag (common for unknown)",
			lat:          0.0,
			lng:          0.0,
			expectFlag:   false,
			expectedFlag: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detector := NewSpoofDetector()

			drone := &Drone{
				Identifier:   "test-coords",
				MACAddress:   "60:60:1F:AA:BB:CC",
				SerialNumber: "1581F5FKD229400001",
				Latitude:     tc.lat,
				Longitude:    tc.lng,
			}

			_, flags := detector.Analyze(drone)

			hasFlag := containsFlag(flags, tc.expectedFlag)
			if tc.expectFlag && !hasFlag {
				t.Errorf("expected flag %q for coords (%.1f, %.1f) but got: %v",
					tc.expectedFlag, tc.lat, tc.lng, flags)
			}
			if !tc.expectFlag && tc.expectedFlag != "" && hasFlag {
				t.Errorf("did not expect flag %q for coords (%.1f, %.1f) but got: %v",
					tc.expectedFlag, tc.lat, tc.lng, flags)
			}
		})
	}
}

// TestConfidenceBonus tests that high-confidence detections get trust bonus
func TestConfidenceBonus(t *testing.T) {
	tests := []struct {
		name          string
		confidence    float32
		expectBonus   bool
		bonusAmount   int
	}{
		{
			name:        "High confidence >= 0.90",
			confidence:  0.95,
			expectBonus: true,
			bonusAmount: 10,
		},
		{
			name:        "Medium confidence >= 0.80",
			confidence:  0.85,
			expectBonus: true,
			bonusAmount: 5,
		},
		{
			name:        "Low confidence < 0.80",
			confidence:  0.70,
			expectBonus: false,
			bonusAmount: 0,
		},
		{
			name:        "Very low confidence (flagged)",
			confidence:  0.40,
			expectBonus: false,
			bonusAmount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detector := NewSpoofDetector()

			drone := &Drone{
				Identifier:   "test-confidence",
				MACAddress:   "60:60:1F:AA:BB:CC",
				SerialNumber: "1581F5FKD229400001",
				Latitude:     37.7749,
				Longitude:    -122.4194,
				Confidence:   tc.confidence,
			}

			trust, _ := detector.Analyze(drone)

			// Base trust is 100, with bonus it should be capped at 100
			// but without serial penalty (10 points) which this test has a serial
			if tc.expectBonus {
				if trust < 100 {
					t.Errorf("expected trust >= 100 with bonus for confidence %.2f but got %d",
						tc.confidence, trust)
				}
			}
		})
	}
}

// TestHaversineDistance tests the haversine distance calculation
func TestHaversineDistance(t *testing.T) {
	tests := []struct {
		name      string
		lat1, lng1 float64
		lat2, lng2 float64
		minKM      float64
		maxKM      float64
	}{
		{
			name: "SFO to Oakland (~18km)",
			lat1: 37.6213, lng1: -122.3790,
			lat2: 37.7213, lng2: -122.2208,
			minKM: 17.0, maxKM: 19.0,
		},
		{
			name: "Same point",
			lat1: 37.7749, lng1: -122.4194,
			lat2: 37.7749, lng2: -122.4194,
			minKM: 0.0, maxKM: 0.001,
		},
		{
			name: "100m north",
			lat1: 37.7749, lng1: -122.4194,
			lat2: 37.7758, lng2: -122.4194, // ~100m
			minKM: 0.09, maxKM: 0.11,
		},
		{
			name: "10km north",
			lat1: 37.7749, lng1: -122.4194,
			lat2: 37.8649, lng2: -122.4194, // ~10km
			minKM: 9.5, maxKM: 10.5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dist := haversineKM(tc.lat1, tc.lng1, tc.lat2, tc.lng2)
			if dist < tc.minKM || dist > tc.maxKM {
				t.Errorf("expected distance %.2f-%.2f km but got %.4f km",
					tc.minKM, tc.maxKM, dist)
			}
		})
	}
}

// TestLocallyAdministeredMAC tests LAA bit detection
func TestLocallyAdministeredMAC(t *testing.T) {
	tests := []struct {
		mac    string
		expect bool
	}{
		{"02:00:00:00:00:00", true},
		{"06:00:00:00:00:00", true},
		{"0A:00:00:00:00:00", true},
		{"0E:00:00:00:00:00", true},
		{"00:00:00:00:00:00", false},
		{"04:00:00:00:00:00", false},
		{"60:60:1F:AA:BB:CC", false}, // DJI
		{"DA:12:34:56:78:9A", true},  // D=1101 but A has LAA set
		{"", false},
	}

	for _, tc := range tests {
		t.Run(tc.mac, func(t *testing.T) {
			got := intel.IsLocallyAdministeredMAC(tc.mac)
			if got != tc.expect {
				t.Errorf("IsLocallyAdministeredMAC(%s) = %v, want %v", tc.mac, got, tc.expect)
			}
		})
	}
}

// TestCleanup tests the track cleanup functionality
func TestCleanup(t *testing.T) {
	detector := NewSpoofDetector()

	// Add some tracks
	for i := 0; i < 5; i++ {
		drone := &Drone{
			Identifier:   "drone-" + string(rune('A'+i)),
			MACAddress:   "60:60:1F:AA:BB:CC",
			SerialNumber: "1581F5FKD229400001",
		}
		detector.Analyze(drone)
	}

	// Age some tracks
	detector.modifyTrack("drone-A", func(track *droneTrack) {
		track.lastTime = time.Now().Add(-1 * time.Hour)
	})
	detector.modifyTrack("drone-B", func(track *droneTrack) {
		track.lastTime = time.Now().Add(-1 * time.Hour)
	})

	// Cleanup with 30 minute threshold
	removed := detector.Cleanup(30 * time.Minute)

	if removed != 2 {
		t.Errorf("expected 2 tracks removed, got %d", removed)
	}

	remaining := detector.trackCount()

	if remaining != 3 {
		t.Errorf("expected 3 tracks remaining, got %d", remaining)
	}
}

// Helper function to check if a flag is in the list
func containsFlag(flags []string, flag string) bool {
	if flag == "" {
		return false
	}
	for _, f := range flags {
		if f == flag {
			return true
		}
	}
	return false
}

// TestDuplicateID tests detection of same identifier appearing from different MACs
// at different locations (potential spoofing or ID collision)
func TestDuplicateID(t *testing.T) {
	tests := []struct {
		name       string
		mac1       string
		mac2       string
		lat1, lng1 float64
		lat2, lng2 float64
		expectFlag bool
	}{
		{
			name:       "Same ID from different MACs, far apart (>100m) - spoof",
			mac1:       "60:60:1F:AA:BB:CC",
			mac2:       "60:60:1F:DD:EE:FF",
			lat1:       37.7749,
			lng1:       -122.4194,
			lat2:       37.7849, // ~1.1km north
			lng2:       -122.4194,
			expectFlag: true,
		},
		{
			name:       "Same ID from different MACs, close together (<100m) - OK",
			mac1:       "60:60:1F:11:22:33",
			mac2:       "60:60:1F:44:55:66",
			lat1:       37.7749,
			lng1:       -122.4194,
			lat2:       37.7750, // ~10m north
			lng2:       -122.4194,
			expectFlag: false,
		},
		{
			name:       "Same ID from same MAC (normal update) - OK",
			mac1:       "60:60:1F:77:88:99",
			mac2:       "60:60:1F:77:88:99",
			lat1:       37.7749,
			lng1:       -122.4194,
			lat2:       37.7849,
			lng2:       -122.4194,
			expectFlag: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detector := NewSpoofDetector()
			identifier := "DUPE-TEST-" + tt.name

			// First detection
			d1 := &Drone{
				Identifier: identifier,
				MACAddress: tt.mac1,
				Latitude:   tt.lat1,
				Longitude:  tt.lng1,
			}
			detector.Analyze(d1)

			// Second detection with potentially different MAC
			d2 := &Drone{
				Identifier: identifier,
				MACAddress: tt.mac2,
				Latitude:   tt.lat2,
				Longitude:  tt.lng2,
			}
			_, flags := detector.Analyze(d2)

			hasFlag := containsFlag(flags, "duplicate_id")
			if hasFlag != tt.expectFlag {
				t.Errorf("duplicate_id flag: got %v, want %v (flags: %v)",
					hasFlag, tt.expectFlag, flags)
			}
		})
	}
}
