package intel

import (
	"math"
	"testing"
)

func TestTDOAToDistanceDiff(t *testing.T) {
	tests := []struct {
		name     string
		deltaNs  int64
		wantM    float64
		tolerance float64
	}{
		{"zero", 0, 0, 0.001},
		{"1 microsecond", 1000, 299.792, 0.1},
		{"negative 1 microsecond", -1000, -299.792, 0.1},
		{"1 millisecond", 1000000, 299792.458, 1.0},
		{"10 nanoseconds", 10, 2.998, 0.01},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TDOAToDistanceDiff(tc.deltaNs)
			if math.Abs(got-tc.wantM) > tc.tolerance {
				t.Errorf("TDOAToDistanceDiff(%d) = %f, want %f (±%f)", tc.deltaNs, got, tc.wantM, tc.tolerance)
			}
		})
	}
}

func TestHyperbolaPoints(t *testing.T) {
	// Two TAPs 500m apart, drone closer to TAP1
	m := TDOAMeasurement{
		Tap1ID:  "tap-001",
		Tap1Lat: 18.253187,
		Tap1Lon: -65.644547,
		Tap2ID:  "tap-002",
		Tap2Lat: 18.252459,
		Tap2Lon: -65.640278,
		DeltaNs: -1000, // TAP2 saw it first (1μs earlier)
		TimingErr: 100,  // 100ns uncertainty
	}

	points := HyperbolaPoints(m, 50)
	if len(points) == 0 {
		t.Fatal("Expected hyperbola points, got none")
	}
	if len(points) != 50 {
		t.Errorf("Expected 50 points, got %d", len(points))
	}

	// All points should be valid lat/lon
	for i, pt := range points {
		if pt[0] < -90 || pt[0] > 90 {
			t.Errorf("Point %d has invalid latitude %f", i, pt[0])
		}
		if pt[1] < -180 || pt[1] > 180 {
			t.Errorf("Point %d has invalid longitude %f", i, pt[1])
		}
	}
}

func TestHyperbolaPointsColocatedTaps(t *testing.T) {
	// Co-located TAPs should return nil (can't do TDOA)
	m := TDOAMeasurement{
		Tap1Lat: 18.252459,
		Tap1Lon: -65.640278,
		Tap2Lat: 18.252459,
		Tap2Lon: -65.640278,
		DeltaNs: 500,
	}
	points := HyperbolaPoints(m, 50)
	if points != nil {
		t.Error("Expected nil for co-located TAPs")
	}
}

func TestHyperbolaPointsInvalidDelta(t *testing.T) {
	// Delta too large (distance diff > baseline) should return nil
	m := TDOAMeasurement{
		Tap1Lat: 18.253187,
		Tap1Lon: -65.644547,
		Tap2Lat: 18.252459,
		Tap2Lon: -65.640278,
		DeltaNs: 10000000, // 10ms = 3000km, way more than baseline
	}
	points := HyperbolaPoints(m, 50)
	if points != nil {
		t.Error("Expected nil for invalid delta (exceeds baseline)")
	}
}

func TestFuseRSSIAndTDOA(t *testing.T) {
	tdoa := TDOAMeasurement{
		Tap1ID:  "tap-001",
		Tap1Lat: 18.253187,
		Tap1Lon: -65.644547,
		Tap2ID:  "tap-003",
		Tap2Lat: 18.252459,
		Tap2Lon: -65.640278,
		DeltaNs: -500, // 500ns, TAP2 first
		TimingErr: 100,
	}

	rings := []TapDistance{
		{TapID: "tap-001", Latitude: 18.253187, Longitude: -65.644547, DistanceM: 800, UncertaintyM: 200},
		{TapID: "tap-003", Latitude: 18.252459, Longitude: -65.640278, DistanceM: 600, UncertaintyM: 150},
	}

	result := FuseRSSIAndTDOA(tdoa, rings)
	if result == nil {
		t.Fatal("Expected fused result, got nil")
	}

	// Result should be a valid position near the TAPs
	if result.Latitude < 18.24 || result.Latitude > 18.27 {
		t.Errorf("Latitude %f outside expected range", result.Latitude)
	}
	if result.Longitude < -65.66 || result.Longitude > -65.62 {
		t.Errorf("Longitude %f outside expected range", result.Longitude)
	}
	if result.Confidence <= 0 || result.Confidence > 1 {
		t.Errorf("Confidence %f outside [0,1]", result.Confidence)
	}
	if result.Method != "tdoa_rssi_fused" {
		t.Errorf("Expected method tdoa_rssi_fused, got %s", result.Method)
	}
}

func TestMatchBurstTimestamps(t *testing.T) {
	base := int64(1700000000000000000) // Some nanosecond timestamp

	tests := []struct {
		name     string
		ts1, ts2 int64
		maxMs    float64
		want     bool
	}{
		{"same time", base, base, 1.0, true},
		{"0.5ms apart", base, base + 500000, 1.0, true},
		{"2ms apart within 5ms window", base, base + 2000000, 5.0, true},
		{"10ms apart outside 5ms window", base, base + 10000000, 5.0, false},
		{"reversed order", base + 500000, base, 1.0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchBurstTimestamps(tc.ts1, tc.ts2, tc.maxMs)
			if got != tc.want {
				t.Errorf("MatchBurstTimestamps(%d, %d, %f) = %v, want %v", tc.ts1, tc.ts2, tc.maxMs, got, tc.want)
			}
		})
	}
}

func TestRFMerger(t *testing.T) {
	m := NewRFMerger()

	// No correlation yet
	if id := m.TryMerge("rf:5745000:20mhz"); id != "" {
		t.Errorf("Expected empty, got %s", id)
	}

	// Record a correlation
	m.RecordCorrelation("rf:5745000:20mhz", "60:60:1F:AA:BB:CC", 5745, 20, 0.6)

	// Should find it now
	if id := m.TryMerge("rf:5745000:20mhz"); id != "60:60:1F:AA:BB:CC" {
		t.Errorf("Expected 60:60:1F:AA:BB:CC, got %s", id)
	}

	// Reverse lookup
	if rfID := m.GetRFForWiFi("60:60:1F:AA:BB:CC"); rfID != "rf:5745000:20mhz" {
		t.Errorf("Expected rf:5745000:20mhz, got %s", rfID)
	}

	// Cleanup with short age should remove it
	removed := m.Cleanup(0)
	if removed != 1 {
		t.Errorf("Expected 1 removed, got %d", removed)
	}
	if id := m.TryMerge("rf:5745000:20mhz"); id != "" {
		t.Errorf("Expected empty after cleanup, got %s", id)
	}
}

func TestShouldCorrelate(t *testing.T) {
	base := int64(1700000000000000000)

	tests := []struct {
		name      string
		wifiTs    int64
		rfTs      int64
		wifiRSSI  int32
		rfPower   int32
		rfProto   RFProtocol
		wifiMfr   string
		want      bool
	}{
		{"same time similar RSSI DJI",
			base, base + 100000000, // 100ms apart
			-55, -60, RFProtoDJIOcuSync, "DJI", true},
		{"too far apart in time",
			base, base + 3000000000, // 3s apart
			-55, -60, RFProtoDJIOcuSync, "DJI", false},
		{"manufacturer mismatch",
			base, base + 100000000,
			-55, -60, RFProtoDJIOcuSync, "Parrot", false},
		{"RSSI too different",
			base, base + 100000000,
			-30, -95, RFProtoDJIOcuSync, "DJI", false},
		{"FPV analog unknown manufacturer ok",
			base, base + 500000000,
			-60, -65, RFProtoAnalogFPV, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldCorrelate(tc.wifiTs, tc.rfTs, tc.wifiRSSI, tc.rfPower, tc.rfProto, tc.wifiMfr)
			if got != tc.want {
				t.Errorf("ShouldCorrelate() = %v, want %v", got, tc.want)
			}
		})
	}
}
