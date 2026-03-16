package geo

import "testing"

func TestLatLonToMGRS(t *testing.T) {
	tests := []struct {
		name      string
		lat, lon  float64
		precision int
		want      string
	}{
		// Known reference points (cross-validated against JS implementation)
		{"White House", 38.8977, -77.0365, 5, "18SUJ 23394 07395"},
		{"Statue of Liberty", 40.6892, -74.0445, 5, "18TWL 80735 04695"},
		{"London", 51.5074, -0.1278, 5, "30UXC 99316 10163"},
		{"Sydney", -33.8688, 151.2093, 5, "56HLH 34368 50948"},
		{"Tokyo", 35.6762, 139.6503, 5, "54SUE 77855 48874"},

		// Precision levels
		{"1m precision", 38.8977, -77.0365, 5, "18SUJ 23394 07395"},
		{"10m precision", 38.8977, -77.0365, 4, "18SUJ 2339 0739"},
		{"100m precision", 38.8977, -77.0365, 3, "18SUJ 233 073"},
		{"1km precision", 38.8977, -77.0365, 2, "18SUJ 23 07"},
		{"10km precision", 38.8977, -77.0365, 1, "18SUJ 2 0"},

		// Edge cases
		{"Sentinel 0,0", 0, 0, 5, ""},
		{"South pole boundary", -80, 0, 5, "31CDM 41867 16915"},
		{"North pole boundary", 84.5, 0, 5, ""},
		{"South beyond range", -81, 0, 5, ""},
		{"Near null island", 0.001, 0.001, 5, "31NAA 66132 00110"},

		// Norway/Svalbard zone exceptions
		{"Norway zone 32", 60, 5, 3, "32VKM 769 581"},
		{"Svalbard zone 33", 78, 15, 3, "33XWG 000 583"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LatLonToMGRS(tt.lat, tt.lon, tt.precision)
			if got != tt.want {
				t.Errorf("LatLonToMGRS(%v, %v, %d) = %q, want %q", tt.lat, tt.lon, tt.precision, got, tt.want)
			}
		})
	}
}

func TestLatLonToMGRSCompact(t *testing.T) {
	got := LatLonToMGRSCompact(38.8977, -77.0365, 5)
	want := "18SUJ2339407395"
	if got != want {
		t.Errorf("LatLonToMGRSCompact = %q, want %q", got, want)
	}

	got = LatLonToMGRSCompact(0, 0, 5)
	if got != "" {
		t.Errorf("LatLonToMGRSCompact(0,0) = %q, want empty", got)
	}
}
