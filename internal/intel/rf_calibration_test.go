package intel

import (
	"math"
	"testing"
)

func TestEstimateRFDistance(t *testing.T) {
	tests := []struct {
		name     string
		power    int32
		proto    RFProtocol
		wantMin  float64 // Distance should be >= this
		wantMax  float64 // Distance should be <= this
	}{
		// DJI OcuSync: RSSI0=26dBm, n=2.4
		// d = 10^((26-(-40))/(10*2.4)) = 10^(66/24) = 10^2.75 ≈ 562m
		{"DJI OcuSync close range", -40, RFProtoDJIOcuSync, 400, 800},
		// d = 10^((26-(-65))/(10*2.4)) = 10^(91/24) = 10^3.79 ≈ 6190m
		{"DJI OcuSync medium range", -65, RFProtoDJIOcuSync, 4000, 10000},
		// Analog FPV: RSSI0=27.8dBm, n=2.2
		// d = 10^((27.8-(-35))/(10*2.2)) = 10^(62.8/22) = 10^2.85 ≈ 715m
		{"Analog FPV 600mW medium", -35, RFProtoAnalogFPV, 500, 1000},
		// ELRS 900MHz: RSSI0=30dBm, n=2.0 — long range, clamped at 40km
		{"ELRS 900MHz long range", -90, RFProtoELRS900, 10000, 50000},
		// Unknown protocol with moderate signal
		{"Unknown protocol", -60, RFProtoUnknown, 100, 5000},
		// Very strong but still within model (RSSI0=26, -20 is close)
		{"Strong signal close", -20, RFProtoDJIOcuSync, 50, 150},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dist, minD, maxD, conf := EstimateRFDistance(tc.power, tc.proto)

			if dist < tc.wantMin || dist > tc.wantMax {
				t.Errorf("distance %f outside expected range [%f, %f]", dist, tc.wantMin, tc.wantMax)
			}
			if minD > dist {
				t.Errorf("min %f should be <= distance %f", minD, dist)
			}
			if maxD < dist {
				t.Errorf("max %f should be >= distance %f", maxD, dist)
			}
			if conf <= 0 || conf > 1 {
				t.Errorf("confidence %f outside (0, 1]", conf)
			}
		})
	}
}

func TestEstimateRFDistanceStrongerThanRSSI0(t *testing.T) {
	// Signal stronger than reference (very close to transmitter)
	dist, _, _, conf := EstimateRFDistance(30, RFProtoDJIOcuSync) // 30 dBm > RSSI0 of 26
	// Should clamp to MinDistM (10m) since RSSI_diff <= 0
	if dist > 15 {
		t.Errorf("Expected clamped distance near 10, got %f", dist)
	}
	if conf <= 0 {
		t.Error("Expected positive confidence")
	}
}

func TestIdentifyRFProtocol(t *testing.T) {
	tests := []struct {
		name  string
		freq  float64
		bw    float64
		mod   string
		want  RFProtocol
	}{
		{"DJI DroneID 2.4GHz", 2414.5, 10, "ofdm", RFProtoDJIDroneID},
		{"DJI OcuSync 5.8GHz 10MHz", 5745, 10, "ofdm", RFProtoDJIOcuSync},
		{"DJI OcuSync O3 40MHz", 5745, 40, "ofdm", RFProtoDJIOcuSync},
		{"Digital FPV 20MHz", 5800, 20, "ofdm", RFProtoDigitalFPV},
		{"Analog FPV FM", 5865, 20, "fm", RFProtoAnalogFPV},
		{"ELRS 900MHz", 915, 0.5, "lora", RFProtoELRS900},
		{"ELRS 2.4GHz", 2440, 0.5, "lora", RFProtoELRS2400},
		{"Generic 2.4GHz FHSS", 2440, 2, "fhss", RFProtoControlLink},
		{"Unknown 1.3GHz", 1300, 30, "fm", RFProtoUnknown},
		{"Default 5.8GHz unknown mod", 5800, 15, "unknown", RFProtoAnalogFPV},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IdentifyRFProtocol(tc.freq, tc.bw, tc.mod)
			if got != tc.want {
				t.Errorf("IdentifyRFProtocol(%f, %f, %s) = %s, want %s",
					tc.freq, tc.bw, tc.mod, got, tc.want)
			}
		})
	}
}

func TestGenerateSyntheticID(t *testing.T) {
	tests := []struct {
		name  string
		freq  float64
		bw    float64
		proto RFProtocol
		want  string
	}{
		{"FPV raceband R1", 5658, 20, RFProtoAnalogFPV, "fpv:R1"},
		{"FPV band A1", 5865, 20, RFProtoAnalogFPV, "fpv:A1"},
		{"DJI DroneID", 2414.5, 10, RFProtoDJIDroneID, "dji_droneid:2414500"},
		{"OcuSync", 5745, 20, RFProtoDJIOcuSync, "ocusync:5745000"},
		{"Generic RF", 915, 5, RFProtoUnknown, "rf:915000:5mhz"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := GenerateSyntheticID(tc.freq, tc.bw, tc.proto)
			if got != tc.want {
				t.Errorf("GenerateSyntheticID(%f, %f, %s) = %s, want %s",
					tc.freq, tc.bw, tc.proto, got, tc.want)
			}
		})
	}
}

func TestGenerateSyntheticIDQuantization(t *testing.T) {
	// Two slightly different measurements should produce the same ID
	id1 := GenerateSyntheticID(5865.1, 19.8, RFProtoAnalogFPV)
	id2 := GenerateSyntheticID(5864.9, 20.2, RFProtoAnalogFPV)
	if id1 != id2 {
		t.Errorf("Expected same synthetic ID for similar frequencies, got %s and %s", id1, id2)
	}
}

func TestGetRFCalibration(t *testing.T) {
	// Known protocol should return specific calibration
	cal := GetRFCalibration(RFProtoDJIOcuSync)
	if cal.RSSI0 != 26.0 {
		t.Errorf("Expected RSSI0=26.0 for DJI OcuSync, got %f", cal.RSSI0)
	}

	// Unknown should return generic
	cal = GetRFCalibration("nonexistent")
	if cal.RSSI0 != RFGenericCalibration.RSSI0 {
		t.Errorf("Expected generic RSSI0=%f, got %f", RFGenericCalibration.RSSI0, cal.RSSI0)
	}
}

func TestPathLossConsistency(t *testing.T) {
	// Doubling distance should increase path loss by ~6-8 dB (for n=2.0-2.6)
	// Distance at RSSI = RSSI0 - X should be 10^(X/(10*n))
	cal := GetRFCalibration(RFProtoDJIOcuSync)

	d1, _, _, _ := EstimateRFDistance(-50, RFProtoDJIOcuSync)
	d2, _, _, _ := EstimateRFDistance(-56, RFProtoDJIOcuSync) // 6 dB weaker

	// At n=2.4, 6dB increase → distance ratio = 10^(6/24) = 10^0.25 ≈ 1.78x
	expectedRatio := math.Pow(10, 6.0/(10*cal.PathLossN))
	actualRatio := d2 / d1

	if math.Abs(actualRatio-expectedRatio)/expectedRatio > 0.01 {
		t.Errorf("Path loss inconsistency: ratio %f, expected %f (d1=%f, d2=%f)",
			actualRatio, expectedRatio, d1, d2)
	}
}
