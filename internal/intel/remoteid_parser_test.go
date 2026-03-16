package intel

import (
	"math"
	"testing"
)

// TestLocationMessageByteOffsets verifies ASTM F3411 Location message byte offsets
// These tests would have caught Bug 1: GPS offset bug where lat/lon were read from wrong offsets
func TestLocationMessageByteOffsets(t *testing.T) {
	tests := []struct {
		name        string
		payload     []byte
		wantLat     float64
		wantLon     float64
		wantSpeed   float32
		wantHeading float32
		tolerance   float64 // Acceptable delta for floating point comparison
	}{
		{
			name: "Puerto Rico drone - known good payload",
			// This is a Location message (0x1x) with Puerto Rico coordinates
			// Expected: lat ~18.325, lon ~-65.655
			payload: []byte{
				0x12,       // Message type (Location = 0x1) + protocol version (2)
				0x02,       // Byte 1: SpeedMult=0, EWDir=1(bit1), HeightType=0, Status=0
				0x62,       // Direction = 98, + 180 (EW flag) = 278 degrees
				0x0A,       // Speed (10 * 0.25 = 2.5 m/s, SpeedMult=0)
				0x00,       // Vertical speed (int8, 0 * 0.5 = 0 m/s)
				// Bytes 4-7: Latitude (int32 little-endian)
				// 18.3250389 * 1e7 = 183250389 -> pack('<i') = [0xD5, 0x2D, 0xEC, 0x0A]
				0xD5, 0x2D, 0xEC, 0x0A,
				// Bytes 8-11: Longitude (int32 little-endian)
				// -65.6547851 * 1e7 = -656547851 -> pack('<i') = [0xF5, 0xDF, 0xDD, 0xD8]
				0xF5, 0xDF, 0xDD, 0xD8,
				// Bytes 12-13: Pressure altitude
				0x00, 0x00,
				// Bytes 14-15: Geodetic altitude
				0x9C, 0x08, // (0x089C = 2204) * 0.5 - 1000 = 102m
				// Bytes 16-17: Height AGL
				0xBD, 0x08,
				// Byte 18: Accuracy
				0x3A,
				// Byte 19: Speed/Baro accuracy
				0x02,
				// Bytes 20-21: Timestamp
				0x94, 0x68,
				// Pad to 25 bytes total
				0x00, 0x00,
			},
			wantLat:     18.3250389,
			wantLon:     -65.6547851,
			wantSpeed:   2.5,
			wantHeading: 278.0,
			tolerance:   0.0001,
		},
		{
			name: "Zero coordinates should remain zero",
			payload: []byte{
				0x12,                   // Location message
				0x00,                   // Status
				0x00,                   // Direction
				0x00,                   // Speed
				0x00,                   // Vertical speed
				0x00, 0x00, 0x00, 0x00, // Latitude = 0
				0x00, 0x00, 0x00, 0x00, // Longitude = 0
				0x00, 0x00, // Pressure alt
				0x00, 0x00, // Geo alt
				0x00, 0x00, // Height
				0x00,       // Accuracy
				0x00,       // Speed accuracy
				0x00, 0x00, // Timestamp
				0x00, 0x00, // Padding
			},
			wantLat:     0,
			wantLon:     0,
			wantSpeed:   0,
			wantHeading: 0,
			tolerance:   0.0001,
		},
		{
			name: "Negative latitude (southern hemisphere)",
			payload: []byte{
				0x12,                   // Location message
				0x00,                   // Status
				0x00,                   // Direction
				0x00,                   // Speed
				0x3F,                   // Vertical speed (63 = 0)
				// Latitude: -33.8688 * 1e7 = -338688000 -> [0x00, 0x08, 0xD0, 0xEB]
				0x00, 0x08, 0xD0, 0xEB,
				// Longitude: 151.2093 * 1e7 = 1512093000 -> [0x48, 0xB5, 0x20, 0x5A]
				0x48, 0xB5, 0x20, 0x5A,
				0x00, 0x00, // Pressure alt
				0x00, 0x00, // Geo alt
				0x00, 0x00, // Height
				0x00,       // Accuracy
				0x00,       // Speed accuracy
				0x00, 0x00, // Timestamp
				0x00, 0x00, // Padding
			},
			wantLat:   -33.8688,
			wantLon:   151.2093,
			wantSpeed: 0,
			tolerance: 0.001,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseRemoteIDPayload(tt.payload)
			if err != nil {
				t.Fatalf("ParseRemoteIDPayload() error = %v", err)
			}

			// Check latitude
			if math.Abs(result.Latitude-tt.wantLat) > tt.tolerance {
				t.Errorf("Latitude = %v, want %v (diff: %v)",
					result.Latitude, tt.wantLat, result.Latitude-tt.wantLat)
			}

			// Check longitude
			if math.Abs(result.Longitude-tt.wantLon) > tt.tolerance {
				t.Errorf("Longitude = %v, want %v (diff: %v)",
					result.Longitude, tt.wantLon, result.Longitude-tt.wantLon)
			}

			// Check speed if expected
			if tt.wantSpeed > 0 {
				if math.Abs(float64(result.Speed-tt.wantSpeed)) > 0.5 {
					t.Errorf("Speed = %v, want %v", result.Speed, tt.wantSpeed)
				}
			}

			// Check heading if expected
			if tt.wantHeading > 0 {
				if math.Abs(float64(result.TrackDirection-tt.wantHeading)) > 2.0 {
					t.Errorf("TrackDirection = %v, want %v", result.TrackDirection, tt.wantHeading)
				}
			}
		})
	}
}

// TestSystemMessageByteOffsets verifies ASTM F3411 System message byte offsets for operator location
func TestSystemMessageByteOffsets(t *testing.T) {
	tests := []struct {
		name       string
		payload    []byte
		wantOpLat  float64
		wantOpLon  float64
		tolerance  float64
	}{
		{
			name: "Puerto Rico operator location",
			payload: []byte{
				0x42,       // Message type (System = 0x4) + protocol version (2)
				// Byte 1: Flags: operator location type = 1 (live GNSS)
				0x01,
				// Bytes 2-5: Operator Latitude (parseSystem uses data[1:5])
				// 18.325 * 1e7 = 183250000 -> [0x50, 0x2C, 0xEC, 0x0A]
				0x50, 0x2C, 0xEC, 0x0A,
				// Bytes 6-9: Operator Longitude (parseSystem uses data[5:9])
				// -65.655 * 1e7 = -656550000 -> [0x90, 0xD7, 0xDD, 0xD8]
				0x90, 0xD7, 0xDD, 0xD8,
				// Bytes 10-11: Area count
				0x01, 0x00,
				// Bytes 12-13: Area radius
				0x00, 0x00,
				// Bytes 14-15: Area ceiling
				0x00, 0x00,
				// Bytes 16-17: Area floor
				0x00, 0x00,
				// Byte 18: Category/Class
				0x00,
				// Bytes 19-20: Operator altitude
				0x00, 0x00,
				// Pad to 25 bytes total
				0x00, 0x00, 0x00, 0x00,
			},
			wantOpLat:  18.325,
			wantOpLon:  -65.655,
			tolerance:  0.001,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseRemoteIDPayload(tt.payload)
			if err != nil {
				t.Fatalf("ParseRemoteIDPayload() error = %v", err)
			}

			if math.Abs(result.OperatorLat-tt.wantOpLat) > tt.tolerance {
				t.Errorf("OperatorLat = %v, want %v", result.OperatorLat, tt.wantOpLat)
			}

			if math.Abs(result.OperatorLon-tt.wantOpLon) > tt.tolerance {
				t.Errorf("OperatorLon = %v, want %v", result.OperatorLon, tt.wantOpLon)
			}
		})
	}
}

// TestNANFrameSkip verifies NAN message counter byte detection and skipping
// This test would have caught Bug 2: NAN framing where first byte needed to be skipped
func TestNANFrameSkip(t *testing.T) {
	tests := []struct {
		name           string
		payload        []byte
		wantSerial     string
		shouldSkipByte bool
	}{
		{
			name: "NAN-wrapped BasicID with 0x5F prefix",
			// 0x5F is invalid for ASTM (type=5, ver=15) - should skip
			payload: []byte{
				0x5F, // NAN marker - should be skipped
				0x02, // BasicID message type (0) + version (2)
				0x10, // ID type (1=serial) + UA type (0=none)
				// Serial number "TEST123" padded with nulls
				'T', 'E', 'S', 'T', '1', '2', '3', 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, // Pad to 25 bytes total from BasicID start
			},
			wantSerial:     "TEST123",
			shouldSkipByte: true,
		},
		{
			name: "Direct ASTM BasicID without NAN wrapper",
			payload: []byte{
				0x02, // BasicID message type (0) + version (2)
				0x10, // ID type (1=serial) + UA type (0=none)
				// Serial number "DIRECT99" padded with nulls
				'D', 'I', 'R', 'E', 'C', 'T', '9', '9', 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, // Pad to 25 bytes
			},
			wantSerial:     "DIRECT99",
			shouldSkipByte: false,
		},
		{
			name: "NAN-wrapped with 0x53 prefix (common)",
			// 0x53 = type 5, ver 3 - invalid ASTM, should skip
			payload: []byte{
				0x53, // NAN marker
				0x02, // BasicID
				0x10,
				'N', 'A', 'N', 'T', 'E', 'S', 'T', 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0,
			},
			wantSerial:     "NANTEST",
			shouldSkipByte: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseRemoteIDPayload(tt.payload)
			if err != nil {
				t.Fatalf("ParseRemoteIDPayload() error = %v", err)
			}

			if result.SerialNumber != tt.wantSerial {
				t.Errorf("SerialNumber = %q, want %q", result.SerialNumber, tt.wantSerial)
			}
		})
	}
}

// TestBasicIDParsing verifies BasicID message parsing
func TestBasicIDParsing(t *testing.T) {
	tests := []struct {
		name       string
		payload    []byte
		wantSerial string
		wantIDType IDType
		wantUAType UAType
	}{
		{
			name: "DJI serial number format",
			payload: []byte{
				0x02, // BasicID + version 2
				0x12, // ID type = 1 (serial), UA type = 2 (helicopter/multirotor)
				'1', '5', '8', '1', 'F', '6', '7', 'Q', 'E', '2',
				'3', '8', '7', '0', '0', 'A', '0', '0', 'K', 'R',
				0, 0, 0, 0, // Padding to 25 bytes
			},
			wantSerial: "1581F67QE238700A00KR",
			wantIDType: IDTypeSerialNumber,
			wantUAType: UATypeHelicopter,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseRemoteIDPayload(tt.payload)
			if err != nil {
				t.Fatalf("ParseRemoteIDPayload() error = %v", err)
			}

			if result.SerialNumber != tt.wantSerial {
				t.Errorf("SerialNumber = %q, want %q", result.SerialNumber, tt.wantSerial)
			}
			if result.IDType != tt.wantIDType {
				t.Errorf("IDType = %v, want %v", result.IDType, tt.wantIDType)
			}
			if result.UAType != tt.wantUAType {
				t.Errorf("UAType = %v, want %v", result.UAType, tt.wantUAType)
			}
		})
	}
}

// TestMessagePackParsing verifies MessagePack (multiple messages) parsing
func TestMessagePackParsing(t *testing.T) {
	// MessagePack with BasicID + Location
	// Each message is exactly 25 bytes per ASTM F3411
	payload := []byte{
		0xF2, // MessagePack (0xF) + version (2)
		0x19, // Message size = 25
		0x02, // Message count = 2
		// Message 1: BasicID (25 bytes total)
		0x02, // Byte 0: BasicID (type 0) + ver 2
		0x12, // Byte 1: ID type (1=serial) + UA type (2=helicopter)
		// Bytes 2-21: Serial number (20 bytes, null-padded)
		'S', 'E', 'R', 'I', 'A', 'L', '1', '2', '3', 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		// Bytes 22-24: Reserved (3 bytes)
		0, 0, 0,
		// Message 2: Location (25 bytes total)
		0x12,       // Byte 0: Location (type 1) + ver 2
		0x00,       // Byte 1: Status
		0x5A,       // Byte 2: Direction (90 * 2 = 180 degrees)
		0x14,       // Byte 3: Speed (20 * 0.25 = 5 m/s)
		0x3F,       // Byte 4: VSpeed (63 = 0 centered)
		// Bytes 5-8: Latitude
		0xD5, 0x2D, 0xEC, 0x0A,
		// Bytes 9-12: Longitude
		0xF5, 0xDF, 0xDD, 0xD8,
		// Bytes 13-14: Pressure alt
		0x00, 0x00,
		// Bytes 15-16: Geo alt
		0x9C, 0x08,
		// Bytes 17-18: Height
		0x00, 0x00,
		// Byte 19: Accuracy
		0x00,
		// Byte 20: Speed accuracy
		0x00,
		// Bytes 21-22: Timestamp
		0x00, 0x00,
		// Bytes 23-24: Reserved
		0x00, 0x00,
	}

	result, err := ParseRemoteIDPayload(payload)
	if err != nil {
		t.Fatalf("ParseRemoteIDPayload() error = %v", err)
	}

	// Should have parsed both messages
	if len(result.MessageTypes) != 2 {
		t.Errorf("MessageTypes count = %d, want 2", len(result.MessageTypes))
	}

	// Check BasicID data
	if result.SerialNumber != "SERIAL123" {
		t.Errorf("SerialNumber = %q, want SERIAL123", result.SerialNumber)
	}

	// Check Location data
	if math.Abs(result.Latitude-18.3250389) > 0.001 {
		t.Errorf("Latitude = %v, want ~18.325", result.Latitude)
	}
}

// TestHasValidPosition verifies the position validation logic
func TestHasValidPosition(t *testing.T) {
	tests := []struct {
		name  string
		lat   float64
		lon   float64
		valid bool
	}{
		{"Valid coordinates", 18.325, -65.655, true},
		{"Zero coordinates", 0, 0, false},
		{"Invalid latitude >90", 91.0, 0, false},
		{"Invalid latitude <-90", -91.0, 0, false},
		{"Invalid longitude >180", 0, 181.0, false},
		{"Invalid longitude <-180", 0, -181.0, false},
		{"Equator valid", 0.001, 0.001, true},
		{"Extreme valid", 89.999, 179.999, true},
		{"Negative valid", -45.0, -120.0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &ParsedRemoteID{
				Latitude:  tt.lat,
				Longitude: tt.lon,
			}
			if got := p.HasValidPosition(); got != tt.valid {
				t.Errorf("HasValidPosition() = %v, want %v", got, tt.valid)
			}
		})
	}
}

// BenchmarkParseRemoteIDPayload benchmarks the parser performance
func BenchmarkParseRemoteIDPayload(b *testing.B) {
	// MessagePack with 2 messages (typical real-world payload)
	payload := []byte{
		0xF2, 0x19, 0x02,
		0x02, 0x12, 'S', 'E', 'R', 'I', 'A', 'L', '1', '2', '3', 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0x12, 0x00, 0x5A, 0x14, 0x3F,
		0xD5, 0x55, 0xEC, 0x0A,
		0x48, 0xBE, 0xDD, 0xD8,
		0x00, 0x00, 0x9C, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseRemoteIDPayload(payload)
	}
}
