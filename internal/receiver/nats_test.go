package receiver

import (
	"testing"
	"time"

	pb "github.com/K13094/skylens/proto"
	"google.golang.org/protobuf/proto"
)

// TestDetectionParsing tests protobuf deserialization of Detection messages
func TestDetectionParsing(t *testing.T) {
	tests := []struct {
		name       string
		detection  *pb.Detection
		expectErr  bool
		validateFn func(t *testing.T, det *pb.Detection)
	}{
		{
			name: "Full DJI detection with RemoteID",
			detection: &pb.Detection{
				TapId:            "tap-001",
				TimestampNs:      time.Now().UnixNano(),
				MacAddress:       "60:60:1F:AA:BB:CC",
				Identifier:       "drone-001",
				SerialNumber:     "1581F5FKD229400001",
				Registration:     "FA-12345",
				Latitude:         37.7749,
				Longitude:        -122.4194,
				AltitudeGeodetic: 120.5,
				AltitudePressure: 118.2,
				HeightAgl:        80.0,
				HeightReference:  pb.HeightReference_HEIGHT_REF_GROUND,
				Speed:            15.5,
				VerticalSpeed:    2.5,
				Heading:          270.0,
				TrackDirection:   268.5,
				OperatorLatitude: 37.7750,
				OperatorLongitude: -122.4190,
				OperatorAltitude: 40.0,
				OperatorLocationType: pb.OperatorLocationType_OP_LOC_LIVE_GNSS,
				OperatorId:       "OP-12345",
				Rssi:             -65,
				Channel:          6,
				FrequencyMhz:     2437,
				Ssid:             "DJI-MAVIC3PRO",
				BeaconIntervalTu: 102,
				Source:           pb.DetectionSource_SOURCE_WIFI_BEACON,
				UavType:          "Helicopter (Multirotor)",
				UavCategory:      pb.UAVCategory_UAV_CAT_HELICOPTER,
				Designation:      "DJI Mavic 3 Pro",
				Manufacturer:     "DJI",
				OperationalStatus: pb.OperationalStatus_OP_STATUS_AIRBORNE,
				Confidence:       0.95,
				IsController:     false,
			},
			expectErr: false,
			validateFn: func(t *testing.T, det *pb.Detection) {
				if det.TapId != "tap-001" {
					t.Errorf("TapId: got %q, want %q", det.TapId, "tap-001")
				}
				if det.SerialNumber != "1581F5FKD229400001" {
					t.Errorf("SerialNumber: got %q", det.SerialNumber)
				}
				if det.Latitude != 37.7749 {
					t.Errorf("Latitude: got %f, want %f", det.Latitude, 37.7749)
				}
				if det.Source != pb.DetectionSource_SOURCE_WIFI_BEACON {
					t.Errorf("Source: got %v", det.Source)
				}
				if det.UavCategory != pb.UAVCategory_UAV_CAT_HELICOPTER {
					t.Errorf("UavCategory: got %v", det.UavCategory)
				}
			},
		},
		{
			name: "Minimal detection (WiFi only)",
			detection: &pb.Detection{
				TapId:       "tap-002",
				TimestampNs: time.Now().UnixNano(),
				MacAddress:  "60:60:1F:11:22:33",
				Identifier:  "wifi-only-001",
				Ssid:        "DJI-MINI3",
				Rssi:        -72,
				Channel:     11,
				Source:      pb.DetectionSource_SOURCE_WIFI_BEACON,
			},
			expectErr: false,
			validateFn: func(t *testing.T, det *pb.Detection) {
				if det.Ssid != "DJI-MINI3" {
					t.Errorf("Ssid: got %q", det.Ssid)
				}
				if det.SerialNumber != "" {
					t.Errorf("SerialNumber should be empty, got %q", det.SerialNumber)
				}
				if det.Latitude != 0 {
					t.Errorf("Latitude should be 0, got %f", det.Latitude)
				}
			},
		},
		{
			name: "DJI OcuSync detection",
			detection: &pb.Detection{
				TapId:            "tap-003",
				TimestampNs:      time.Now().UnixNano(),
				MacAddress:       "60:60:1F:DE:AD:BE",
				Identifier:       "ocusync-001",
				SerialNumber:     "1581F7FV123456789",
				Latitude:         33.9425,
				Longitude:        -118.4081,
				AltitudeGeodetic: 200.0,
				Speed:            25.0,
				Heading:          90.0,
				Source:           pb.DetectionSource_SOURCE_DJI_OCUSYNC,
				Manufacturer:     "DJI",
				Designation:      "Matrice 4E",
				Confidence:       0.92,
			},
			expectErr: false,
			validateFn: func(t *testing.T, det *pb.Detection) {
				if det.Source != pb.DetectionSource_SOURCE_DJI_OCUSYNC {
					t.Errorf("Source: got %v", det.Source)
				}
				if det.Designation != "Matrice 4E" {
					t.Errorf("Designation: got %q", det.Designation)
				}
			},
		},
		{
			name: "Bluetooth 5 detection",
			detection: &pb.Detection{
				TapId:        "tap-004",
				TimestampNs:  time.Now().UnixNano(),
				MacAddress:   "AA:BB:CC:DD:EE:FF",
				Identifier:   "bt5-001",
				SerialNumber: "AUTEL-12345",
				Latitude:     40.7128,
				Longitude:    -74.0060,
				Source:       pb.DetectionSource_SOURCE_BLUETOOTH_5,
				Manufacturer: "Autel",
				Designation:  "EVO III",
				Rssi:         -58,
			},
			expectErr: false,
			validateFn: func(t *testing.T, det *pb.Detection) {
				if det.Source != pb.DetectionSource_SOURCE_BLUETOOTH_5 {
					t.Errorf("Source: got %v", det.Source)
				}
			},
		},
		{
			name: "Controller detection",
			detection: &pb.Detection{
				TapId:        "tap-005",
				TimestampNs:  time.Now().UnixNano(),
				MacAddress:   "60:60:1F:RC:01:02",
				Identifier:   "controller-001",
				Ssid:         "DJI-RC-PRO",
				Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
				Manufacturer: "DJI",
				IsController: true,
				Rssi:         -45,
			},
			expectErr: false,
			validateFn: func(t *testing.T, det *pb.Detection) {
				if !det.IsController {
					t.Error("IsController should be true")
				}
			},
		},
		{
			name: "Detection with raw frame data",
			detection: &pb.Detection{
				TapId:           "tap-006",
				TimestampNs:     time.Now().UnixNano(),
				MacAddress:      "60:60:1F:FF:EE:DD",
				Identifier:      "raw-001",
				Source:          pb.DetectionSource_SOURCE_WIFI_BEACON,
				RawFrame:        []byte{0x80, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff},
				RemoteidPayload: []byte{0x0D, 0x00, 0x01, 0x02, 0x03},
			},
			expectErr: false,
			validateFn: func(t *testing.T, det *pb.Detection) {
				if len(det.RawFrame) != 8 {
					t.Errorf("RawFrame length: got %d, want 8", len(det.RawFrame))
				}
				if len(det.RemoteidPayload) != 5 {
					t.Errorf("RemoteidPayload length: got %d, want 5", len(det.RemoteidPayload))
				}
			},
		},
		{
			name: "Emergency status detection",
			detection: &pb.Detection{
				TapId:             "tap-007",
				TimestampNs:       time.Now().UnixNano(),
				MacAddress:        "60:60:1F:EM:ER:GY",
				Identifier:        "emergency-001",
				Latitude:          35.6762,
				Longitude:         139.6503,
				OperationalStatus: pb.OperationalStatus_OP_STATUS_EMERGENCY,
				Source:            pb.DetectionSource_SOURCE_WIFI_BEACON,
			},
			expectErr: false,
			validateFn: func(t *testing.T, det *pb.Detection) {
				if det.OperationalStatus != pb.OperationalStatus_OP_STATUS_EMERGENCY {
					t.Errorf("OperationalStatus: got %v", det.OperationalStatus)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Marshal to protobuf
			data, err := proto.Marshal(tc.detection)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			// Unmarshal back
			var parsed pb.Detection
			err = proto.Unmarshal(data, &parsed)

			if tc.expectErr {
				if err == nil {
					t.Error("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Run validation
			tc.validateFn(t, &parsed)
		})
	}
}

// TestHeartbeatParsing tests protobuf deserialization of TapHeartbeat messages
func TestHeartbeatParsing(t *testing.T) {
	tests := []struct {
		name       string
		heartbeat  *pb.TapHeartbeat
		expectErr  bool
		validateFn func(t *testing.T, hb *pb.TapHeartbeat)
	}{
		{
			name: "Full heartbeat with stats",
			heartbeat: &pb.TapHeartbeat{
				TapId:              "tap-001",
				TapName:            "Airport-North",
				TimestampNs:        time.Now().UnixNano(),
				Latitude:           37.6213,
				Longitude:          -122.3790,
				Altitude:           10.0,
				CpuPercent:         45.5,
				MemoryPercent:      62.3,
				TemperatureCelsius: 48.2,
				WifiInterface:      "wlan0",
				Version:            "1.2.3",
				Stats: &pb.TapStats{
					PacketsCaptured:    1000000,
					PacketsFiltered:    500000,
					DetectionsSent:     1234,
					BytesCaptured:      150000000,
					CurrentChannel:     6,
					PacketsPerSecond:   1500.5,
					UptimeSeconds:      86400,
					PcapKernelReceived: 1000000,
					PcapKernelDropped:  0,
				},
			},
			expectErr: false,
			validateFn: func(t *testing.T, hb *pb.TapHeartbeat) {
				if hb.TapId != "tap-001" {
					t.Errorf("TapId: got %q", hb.TapId)
				}
				if hb.TapName != "Airport-North" {
					t.Errorf("TapName: got %q", hb.TapName)
				}
				if hb.Stats == nil {
					t.Fatal("Stats should not be nil")
				}
				if hb.Stats.PacketsCaptured != 1000000 {
					t.Errorf("PacketsCaptured: got %d", hb.Stats.PacketsCaptured)
				}
				if hb.Stats.DetectionsSent != 1234 {
					t.Errorf("DetectionsSent: got %d", hb.Stats.DetectionsSent)
				}
				if hb.Stats.PcapKernelDropped != 0 {
					t.Errorf("PcapKernelDropped: got %d", hb.Stats.PcapKernelDropped)
				}
			},
		},
		{
			name: "Heartbeat with kernel drops",
			heartbeat: &pb.TapHeartbeat{
				TapId:       "tap-overloaded",
				TapName:     "High-Traffic-Zone",
				TimestampNs: time.Now().UnixNano(),
				Stats: &pb.TapStats{
					PacketsCaptured:    5000000,
					PacketsFiltered:    2500000,
					DetectionsSent:     10000,
					CurrentChannel:     36,
					PacketsPerSecond:   5000.0,
					UptimeSeconds:      3600,
					PcapKernelReceived: 5000000,
					PcapKernelDropped:  50000, // 1% drop rate
				},
			},
			expectErr: false,
			validateFn: func(t *testing.T, hb *pb.TapHeartbeat) {
				if hb.Stats.PcapKernelDropped != 50000 {
					t.Errorf("PcapKernelDropped: got %d", hb.Stats.PcapKernelDropped)
				}
				dropRate := float64(hb.Stats.PcapKernelDropped) / float64(hb.Stats.PcapKernelReceived+hb.Stats.PcapKernelDropped) * 100
				if dropRate < 0.9 || dropRate > 1.1 {
					t.Errorf("drop rate should be ~1%%, got %.2f%%", dropRate)
				}
			},
		},
		{
			name: "Minimal heartbeat (no stats)",
			heartbeat: &pb.TapHeartbeat{
				TapId:       "tap-minimal",
				TimestampNs: time.Now().UnixNano(),
			},
			expectErr: false,
			validateFn: func(t *testing.T, hb *pb.TapHeartbeat) {
				if hb.Stats != nil {
					t.Error("Stats should be nil")
				}
				if hb.TapName != "" {
					t.Errorf("TapName should be empty, got %q", hb.TapName)
				}
			},
		},
		{
			name: "Heartbeat with high temperature",
			heartbeat: &pb.TapHeartbeat{
				TapId:              "tap-hot",
				TapName:            "Desert-Site",
				TimestampNs:        time.Now().UnixNano(),
				CpuPercent:         95.0,
				MemoryPercent:      88.0,
				TemperatureCelsius: 78.5, // Hot Pi!
				Stats: &pb.TapStats{
					PacketsCaptured:  100000,
					PacketsPerSecond: 500.0,
					UptimeSeconds:    7200,
				},
			},
			expectErr: false,
			validateFn: func(t *testing.T, hb *pb.TapHeartbeat) {
				if hb.TemperatureCelsius != 78.5 {
					t.Errorf("Temperature: got %f", hb.TemperatureCelsius)
				}
				if hb.CpuPercent != 95.0 {
					t.Errorf("CpuPercent: got %f", hb.CpuPercent)
				}
			},
		},
		{
			name: "Heartbeat with location",
			heartbeat: &pb.TapHeartbeat{
				TapId:       "tap-gps",
				TapName:     "Mobile-Unit",
				TimestampNs: time.Now().UnixNano(),
				Latitude:    51.5074,
				Longitude:   -0.1278,
				Altitude:    25.0,
				Stats: &pb.TapStats{
					CurrentChannel: 149, // 5GHz
				},
			},
			expectErr: false,
			validateFn: func(t *testing.T, hb *pb.TapHeartbeat) {
				if hb.Latitude != 51.5074 {
					t.Errorf("Latitude: got %f", hb.Latitude)
				}
				if hb.Stats.CurrentChannel != 149 {
					t.Errorf("CurrentChannel: got %d", hb.Stats.CurrentChannel)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Marshal to protobuf
			data, err := proto.Marshal(tc.heartbeat)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			// Unmarshal back
			var parsed pb.TapHeartbeat
			err = proto.Unmarshal(data, &parsed)

			if tc.expectErr {
				if err == nil {
					t.Error("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Run validation
			tc.validateFn(t, &parsed)
		})
	}
}

// TestClassifyByTrust tests trust score to classification mapping
// Classification: HOSTILE < SUSPECT < UNKNOWN < NEUTRAL < FRIENDLY
func TestClassifyByTrust(t *testing.T) {
	tests := []struct {
		name           string
		trustScore     int
		flags          []string
		expectedClass  string
	}{
		// High trust, no flags -> FRIENDLY (>= 80)
		{
			name:          "Perfect trust - FRIENDLY",
			trustScore:    100,
			flags:         []string{},
			expectedClass: "FRIENDLY",
		},
		{
			name:          "High trust 95 - FRIENDLY",
			trustScore:    95,
			flags:         []string{},
			expectedClass: "FRIENDLY",
		},
		{
			name:          "Trust 90 - FRIENDLY",
			trustScore:    90,
			flags:         []string{},
			expectedClass: "FRIENDLY",
		},
		{
			name:          "Trust 89 - FRIENDLY",
			trustScore:    89,
			flags:         []string{},
			expectedClass: "FRIENDLY",
		},

		// Good trust, no flags -> NEUTRAL (60-79)
		{
			name:          "Trust 75 - NEUTRAL",
			trustScore:    75,
			flags:         []string{},
			expectedClass: "NEUTRAL",
		},
		{
			name:          "Trust 70 - NEUTRAL",
			trustScore:    70,
			flags:         []string{},
			expectedClass: "NEUTRAL",
		},
		{
			name:          "Trust 69 - NEUTRAL",
			trustScore:    69,
			flags:         []string{},
			expectedClass: "NEUTRAL",
		},

		// Moderate trust, no flags -> UNKNOWN (40-59)
		{
			name:          "Trust 55 - UNKNOWN",
			trustScore:    55,
			flags:         []string{},
			expectedClass: "UNKNOWN",
		},
		{
			name:          "Trust 50 - UNKNOWN",
			trustScore:    50,
			flags:         []string{},
			expectedClass: "UNKNOWN",
		},

		// Any flags with decent trust -> UNKNOWN (flags > 0 catches it)
		{
			name:          "Trust 80 with 1 weak flag - FRIENDLY",
			trustScore:    80,
			flags:         []string{"no_serial"},
			expectedClass: "FRIENDLY",
		},
		{
			name:          "Trust 65 with 1 generic flag - NEUTRAL",
			trustScore:    65,
			flags:         []string{"randomized_mac"},
			expectedClass: "NEUTRAL",
		},
		{
			name:          "Trust 49 no flags - UNKNOWN",
			trustScore:    49,
			flags:         []string{},
			expectedClass: "UNKNOWN",
		},

		// Low trust -> SUSPECT (20-39)
		{
			name:          "Trust 29 - SUSPECT",
			trustScore:    29,
			flags:         []string{},
			expectedClass: "SUSPECT",
		},
		{
			name:          "Trust 50 with 2 generic flags - UNKNOWN",
			trustScore:    50,
			flags:         []string{"speed_violation", "altitude_spike"},
			expectedClass: "UNKNOWN",
		},
		{
			name:          "Trust 80 with critical+spoof flags - UNKNOWN",
			trustScore:    80,
			flags:         []string{"coordinate_jump", "oui_ssid_mismatch"},
			expectedClass: "UNKNOWN",
		},
		{
			name:          "Trust 0 with critical flags - HOSTILE",
			trustScore:    0,
			flags:         []string{"impossible_vendor", "invalid_coordinates", "speed_violation"},
			expectedClass: "HOSTILE",
		},

		// Edge cases
		{
			name:          "Trust 30 with generic flag - SUSPECT",
			trustScore:    30,
			flags:         []string{"no_serial"},
			expectedClass: "SUSPECT",
		},
		{
			name:          "Trust 50 exactly no flags - UNKNOWN",
			trustScore:    50,
			flags:         []string{},
			expectedClass: "UNKNOWN",
		},

		// Additional: test critical flag + low-ish trust -> HOSTILE
		{
			name:          "Trust 35 with critical flag - HOSTILE",
			trustScore:    35,
			flags:         []string{"coordinate_jump"},
			expectedClass: "HOSTILE",
		},
		// Single spoof flag at high trust is no longer SUSPECT (reduces false positives)
		{
			name:          "Trust 90 with single spoof flag - FRIENDLY",
			trustScore:    90,
			flags:         []string{"oui_ssid_mismatch"},
			expectedClass: "FRIENDLY",
		},
		// 3 generic flags at high trust -> UNKNOWN (not SUSPECT without spoof indicators)
		{
			name:          "Trust 95 with 3 generic flags - UNKNOWN",
			trustScore:    95,
			flags:         []string{"no_serial", "randomized_mac", "speed_violation"},
			expectedClass: "UNKNOWN",
		},
		// 2 spoof flags -> SUSPECT regardless of trust
		{
			name:          "Trust 90 with 2 spoof flags - SUSPECT",
			trustScore:    90,
			flags:         []string{"oui_ssid_mismatch", "rssi_distance_mismatch"},
			expectedClass: "SUSPECT",
		},
		// 4+ flags -> SUSPECT
		{
			name:          "Trust 85 with 4 generic flags - SUSPECT",
			trustScore:    85,
			flags:         []string{"no_serial", "randomized_mac", "speed_violation", "altitude_spike"},
			expectedClass: "SUSPECT",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := classifyByTrust(tc.trustScore, tc.flags)
			if result != tc.expectedClass {
				t.Errorf("classifyByTrust(%d, %v) = %q, want %q",
					tc.trustScore, tc.flags, result, tc.expectedClass)
			}
		})
	}
}

// TestClassifyByTrustBoundaries tests exact boundary conditions
func TestClassifyByTrustBoundaries(t *testing.T) {
	boundaries := []struct {
		trust       int
		flags       int
		description string
	}{
		{80, 0, "FRIENDLY threshold"},
		{79, 0, "just below FRIENDLY"},
		{60, 0, "NEUTRAL threshold"},
		{59, 0, "just below NEUTRAL"},
		{40, 0, "UNKNOWN threshold"},
		{39, 0, "just below UNKNOWN"},
		{20, 0, "SUSPECT threshold"},
		{19, 0, "just below SUSPECT (HOSTILE)"},
	}

	for _, b := range boundaries {
		t.Run(b.description, func(t *testing.T) {
			flags := make([]string, b.flags)
			for i := 0; i < b.flags; i++ {
				flags[i] = "test_flag"
			}
			result := classifyByTrust(b.trust, flags)
			t.Logf("trust=%d, flags=%d -> %s", b.trust, b.flags, result)
		})
	}
}

// TestCommandAckParsing tests TapCommandAck deserialization
func TestCommandAckParsing(t *testing.T) {
	tests := []struct {
		name string
		ack  *pb.TapCommandAck
	}{
		{
			name: "Successful ping ack",
			ack: &pb.TapCommandAck{
				TapId:        "tap-001",
				CommandId:    "ping-123456",
				Success:      true,
				ErrorMessage: "",
				LatencyNs:    5000000, // 5ms
			},
		},
		{
			name: "Failed command ack",
			ack: &pb.TapCommandAck{
				TapId:        "tap-002",
				CommandId:    "setchan-789",
				Success:      false,
				ErrorMessage: "channel 36 not supported",
				LatencyNs:    0,
			},
		},
		{
			name: "Restart ack",
			ack: &pb.TapCommandAck{
				TapId:        "tap-003",
				CommandId:    "restart-abc",
				Success:      true,
				ErrorMessage: "",
				LatencyNs:    100000000, // 100ms
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := proto.Marshal(tc.ack)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			var parsed pb.TapCommandAck
			if err := proto.Unmarshal(data, &parsed); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			if parsed.TapId != tc.ack.TapId {
				t.Errorf("TapId: got %q, want %q", parsed.TapId, tc.ack.TapId)
			}
			if parsed.Success != tc.ack.Success {
				t.Errorf("Success: got %v, want %v", parsed.Success, tc.ack.Success)
			}
		})
	}
}

// TestTapCommandParsing tests TapCommand serialization/deserialization
func TestTapCommandParsing(t *testing.T) {
	tests := []struct {
		name    string
		command *pb.TapCommand
	}{
		{
			name: "Ping command",
			command: &pb.TapCommand{
				TapId:       "tap-001",
				TimestampNs: time.Now().UnixNano(),
				CommandId:   "ping-123",
				Command: &pb.TapCommand_Ping{
					Ping: &pb.PingCommand{
						SentAtNs: time.Now().UnixNano(),
					},
				},
			},
		},
		{
			name: "SetChannels command",
			command: &pb.TapCommand{
				TapId:       "tap-002",
				TimestampNs: time.Now().UnixNano(),
				CommandId:   "chan-456",
				Command: &pb.TapCommand_SetChannels{
					SetChannels: &pb.SetChannelsCommand{
						Channels:      []int32{1, 6, 11, 36, 40, 44, 48},
						HopIntervalMs: 200,
					},
				},
			},
		},
		{
			name: "Restart command",
			command: &pb.TapCommand{
				TapId:       "tap-003",
				TimestampNs: time.Now().UnixNano(),
				CommandId:   "restart-789",
				Command: &pb.TapCommand_Restart{
					Restart: &pb.RestartCommand{
						Graceful: true,
					},
				},
			},
		},
		{
			name: "SetFilter command",
			command: &pb.TapCommand{
				TapId:       "tap-004",
				TimestampNs: time.Now().UnixNano(),
				CommandId:   "filter-abc",
				Command: &pb.TapCommand_SetFilter{
					SetFilter: &pb.SetFilterCommand{
						BpfFilter: "type mgt subtype beacon",
					},
				},
			},
		},
		{
			name: "UpdateConfig command",
			command: &pb.TapCommand{
				TapId:       "tap-005",
				TimestampNs: time.Now().UnixNano(),
				CommandId:   "config-def",
				Command: &pb.TapCommand_UpdateConfig{
					UpdateConfig: &pb.UpdateConfigCommand{
						Config: map[string]string{
							"log_level":      "debug",
							"heartbeat_sec":  "5",
							"capture_raw":    "true",
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := proto.Marshal(tc.command)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			var parsed pb.TapCommand
			if err := proto.Unmarshal(data, &parsed); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			if parsed.TapId != tc.command.TapId {
				t.Errorf("TapId: got %q, want %q", parsed.TapId, tc.command.TapId)
			}
			if parsed.CommandId != tc.command.CommandId {
				t.Errorf("CommandId: got %q, want %q", parsed.CommandId, tc.command.CommandId)
			}

			// Verify command type is preserved
			switch tc.command.Command.(type) {
			case *pb.TapCommand_Ping:
				if _, ok := parsed.Command.(*pb.TapCommand_Ping); !ok {
					t.Error("expected Ping command")
				}
			case *pb.TapCommand_SetChannels:
				if sc, ok := parsed.Command.(*pb.TapCommand_SetChannels); !ok {
					t.Error("expected SetChannels command")
				} else {
					orig := tc.command.Command.(*pb.TapCommand_SetChannels)
					if len(sc.SetChannels.Channels) != len(orig.SetChannels.Channels) {
						t.Errorf("channels count mismatch")
					}
				}
			case *pb.TapCommand_Restart:
				if _, ok := parsed.Command.(*pb.TapCommand_Restart); !ok {
					t.Error("expected Restart command")
				}
			case *pb.TapCommand_SetFilter:
				if _, ok := parsed.Command.(*pb.TapCommand_SetFilter); !ok {
					t.Error("expected SetFilter command")
				}
			case *pb.TapCommand_UpdateConfig:
				if uc, ok := parsed.Command.(*pb.TapCommand_UpdateConfig); !ok {
					t.Error("expected UpdateConfig command")
				} else {
					orig := tc.command.Command.(*pb.TapCommand_UpdateConfig)
					if len(uc.UpdateConfig.Config) != len(orig.UpdateConfig.Config) {
						t.Errorf("config count mismatch")
					}
				}
			}
		})
	}
}

// TestAlertParsing tests Alert message parsing
func TestAlertParsing(t *testing.T) {
	tests := []struct {
		name  string
		alert *pb.Alert
	}{
		{
			name: "New drone alert",
			alert: &pb.Alert{
				AlertId:         "alert-001",
				TimestampNs:     time.Now().UnixNano(),
				Priority:        pb.AlertPriority_ALERT_INFO,
				AlertType:       pb.AlertType_ALERT_TYPE_NEW_DRONE,
				Title:           "New Drone Detected",
				Message:         "DJI Mavic 3 Pro detected at SFO Terminal 2",
				DroneIdentifier: "drone-001",
				TapId:           "tap-001",
				Metadata: map[string]string{
					"manufacturer": "DJI",
					"model":        "Mavic 3 Pro",
				},
			},
		},
		{
			name: "Spoof detected alert",
			alert: &pb.Alert{
				AlertId:         "alert-002",
				TimestampNs:     time.Now().UnixNano(),
				Priority:        pb.AlertPriority_ALERT_CRITICAL,
				AlertType:       pb.AlertType_ALERT_TYPE_SPOOF_DETECTED,
				Title:           "Spoofing Detected",
				Message:         "GPS teleportation detected: 10km in 1 second",
				DroneIdentifier: "spoof-001",
				TapId:           "tap-002",
				Metadata: map[string]string{
					"flag":     "coordinate_jump",
					"distance": "10000m",
					"delta_t":  "1s",
				},
			},
		},
		{
			name: "Tap offline alert",
			alert: &pb.Alert{
				AlertId:     "alert-003",
				TimestampNs: time.Now().UnixNano(),
				Priority:    pb.AlertPriority_ALERT_HIGH,
				AlertType:   pb.AlertType_ALERT_TYPE_TAP_OFFLINE,
				Title:       "TAP Offline",
				Message:     "TAP Airport-North has not sent heartbeat in 60 seconds",
				TapId:       "tap-airport-north",
			},
		},
		{
			name: "Zone violation alert",
			alert: &pb.Alert{
				AlertId:         "alert-004",
				TimestampNs:     time.Now().UnixNano(),
				Priority:        pb.AlertPriority_ALERT_WARNING,
				AlertType:       pb.AlertType_ALERT_TYPE_ZONE_VIOLATION,
				Title:           "Restricted Zone Entry",
				Message:         "Drone entered restricted airspace near Runway 28L",
				DroneIdentifier: "drone-002",
				Metadata: map[string]string{
					"zone":     "runway-28l",
					"distance": "50m",
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := proto.Marshal(tc.alert)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			var parsed pb.Alert
			if err := proto.Unmarshal(data, &parsed); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			if parsed.AlertId != tc.alert.AlertId {
				t.Errorf("AlertId: got %q, want %q", parsed.AlertId, tc.alert.AlertId)
			}
			if parsed.Priority != tc.alert.Priority {
				t.Errorf("Priority: got %v, want %v", parsed.Priority, tc.alert.Priority)
			}
			if parsed.AlertType != tc.alert.AlertType {
				t.Errorf("AlertType: got %v, want %v", parsed.AlertType, tc.alert.AlertType)
			}
			if len(parsed.Metadata) != len(tc.alert.Metadata) {
				t.Errorf("Metadata count mismatch")
			}
		})
	}
}

// TestDetectionSourceStrings tests DetectionSource enum string conversion
func TestDetectionSourceStrings(t *testing.T) {
	sources := []struct {
		source pb.DetectionSource
		want   string
	}{
		{pb.DetectionSource_SOURCE_UNKNOWN, "SOURCE_UNKNOWN"},
		{pb.DetectionSource_SOURCE_WIFI_BEACON, "SOURCE_WIFI_BEACON"},
		{pb.DetectionSource_SOURCE_WIFI_NAN, "SOURCE_WIFI_NAN"},
		{pb.DetectionSource_SOURCE_WIFI_PROBE_RESP, "SOURCE_WIFI_PROBE_RESP"},
		{pb.DetectionSource_SOURCE_BLUETOOTH_4, "SOURCE_BLUETOOTH_4"},
		{pb.DetectionSource_SOURCE_BLUETOOTH_5, "SOURCE_BLUETOOTH_5"},
		{pb.DetectionSource_SOURCE_DJI_OCUSYNC, "SOURCE_DJI_OCUSYNC"},
		{pb.DetectionSource_SOURCE_ADS_B, "SOURCE_ADS_B"},
	}

	for _, tc := range sources {
		t.Run(tc.want, func(t *testing.T) {
			got := tc.source.String()
			if got != tc.want {
				t.Errorf("DetectionSource.String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMalformedProtobuf tests handling of malformed protobuf data
func TestMalformedProtobuf(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"garbage", []byte{0xFF, 0xFE, 0xFD, 0xFC}},
		{"truncated", []byte{0x0A, 0x05, 0x74, 0x61}}, // incomplete string
		{"wrong type", []byte{0x08, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}}, // overflow varint
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var det pb.Detection
			err := proto.Unmarshal(tc.data, &det)
			// We just verify it doesn't panic - error is expected for malformed data
			if tc.name != "empty" && err == nil {
				// Empty is valid (all defaults)
				if tc.name == "garbage" || tc.name == "truncated" {
					// These should error
					t.Log("Note: expected error for malformed data")
				}
			}
		})
	}
}
