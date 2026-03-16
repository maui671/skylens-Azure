package intel

import (
	"strings"
	"testing"
)

// TestMatchSSID_DJI tests SSID pattern matching for DJI drones
// Note: The patterns return either Model (exact match) or ModelHint (category match).
// Tests verify manufacturer and that at least one of model/modelHint is set.
func TestMatchSSID_DJI(t *testing.T) {
	tests := []struct {
		name         string
		ssid         string
		expectMatch  bool
		manufacturer string
		wantModelOrHint string // Either Model or ModelHint should contain this
		isController bool
	}{
		// Mavic series
		{
			name:         "DJI-MAVIC3PRO",
			ssid:         "DJI-MAVIC3PRO",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "", // generic pattern
			isController: false,
		},
		{
			name:         "MAVIC-3-CLASSIC",
			ssid:         "MAVIC-3-CLASSIC",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "Mavic",
			isController: false,
		},
		{
			name:         "MAVIC 4 PRO specific",
			ssid:         "MAVIC-4-PRO",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "Mavic",
			isController: false,
		},

		// Mini series
		{
			name:         "DJI_MINI3PRO",
			ssid:         "DJI_MINI3PRO",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "", // matches generic DJI pattern
			isController: false,
		},
		{
			name:         "MINI-5-PRO",
			ssid:         "MINI-5-PRO",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "Mini",
			isController: false,
		},

		// Air series
		{
			name:         "DJI AIR 3S",
			ssid:         "DJI AIR 3S",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "",
			isController: false,
		},
		{
			name:         "AIR-4S specific",
			ssid:         "AIR-4S",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "Air",
			isController: false,
		},

		// FPV/Avata series
		{
			name:         "DJIFPV no separator",
			ssid:         "DJIFPV",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "FPV",
			isController: false,
		},
		{
			name:         "DJI generic underscore",
			ssid:         "DJI_FPV",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "", // Matches generic DJI pattern first
			isController: false,
		},
		{
			name:         "DJI AVATA underscore",
			ssid:         "DJI_AVATA",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "", // Generic DJI pattern
			isController: false,
		},
		{
			name:         "DJI NEO underscore",
			ssid:         "DJI_NEO",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "", // Generic DJI pattern
			isController: false,
		},

		// Enterprise/Matrice series
		{
			name:         "MATRICE30T",
			ssid:         "MATRICE30T",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "Matrice",
			isController: false,
		},
		{
			name:         "INSPIRE-3",
			ssid:         "INSPIRE-3",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "Inspire",
			isController: false,
		},

		// Agricultural
		{
			name:         "AGRAS T60",
			ssid:         "AGRAST60",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "Agras",
			isController: false,
		},
		{
			name:         "FLYCART 30",
			ssid:         "FLYCART30",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "FlyCart",
			isController: false,
		},

		// Tello
		{
			name:         "TELLO-ABC123",
			ssid:         "TELLO-ABC123",
			expectMatch:  true,
			manufacturer: "DJI/Ryze",
			wantModelOrHint: "Tello",
			isController: false,
		},
		{
			name:         "TELLO alone",
			ssid:         "TELLO",
			expectMatch:  true,
			manufacturer: "DJI/Ryze",
			wantModelOrHint: "Tello",
			isController: false,
		},

		// Legacy
		{
			name:         "PHANTOM-4",
			ssid:         "PHANTOM-4-PRO",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "Phantom",
			isController: false,
		},
		{
			name:         "SPARK-123",
			ssid:         "SPARK-123456",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "Spark",
			isController: false,
		},

		// Controllers
		{
			name:         "DJI RC controller",
			ssid:         "DJI-RC-PRO",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "controller",
			isController: true,
		},
		{
			name:         "DJI Goggles N/2/3 pattern",
			ssid:         "DJIGOGGLES2",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "Goggles",
			isController: true,
		},

		// Case insensitivity
		{
			name:         "lowercase dji mavic",
			ssid:         "dji-mavic3",
			expectMatch:  true,
			manufacturer: "DJI",
			wantModelOrHint: "",
			isController: false,
		},

		// Non-matches
		{
			name:         "Random SSID",
			ssid:         "MyHomeWifi",
			expectMatch:  false,
			manufacturer: "",
			wantModelOrHint: "",
			isController: false,
		},
		{
			name:         "Empty SSID",
			ssid:         "",
			expectMatch:  false,
			manufacturer: "",
			wantModelOrHint: "",
			isController: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := MatchSSID(tc.ssid)

			if tc.expectMatch {
				if result == nil {
					t.Errorf("expected match for SSID %q but got nil", tc.ssid)
					return
				}
				if result.Manufacturer != tc.manufacturer {
					t.Errorf("manufacturer: got %q, want %q", result.Manufacturer, tc.manufacturer)
				}
				// Check that wantModelOrHint appears in either Model or ModelHint
				if tc.wantModelOrHint != "" {
					modelOrHint := result.Model + result.ModelHint
					if !containsIgnoreCase(modelOrHint, tc.wantModelOrHint) {
						t.Errorf("expected Model or ModelHint to contain %q, got Model=%q ModelHint=%q",
							tc.wantModelOrHint, result.Model, result.ModelHint)
					}
				}
				if result.IsController != tc.isController {
					t.Errorf("isController: got %v, want %v", result.IsController, tc.isController)
				}
			} else {
				if result != nil {
					t.Errorf("expected no match for SSID %q but got %+v", tc.ssid, result)
				}
			}
		})
	}
}

// TestMatchSSID_Parrot tests SSID pattern matching for Parrot drones
func TestMatchSSID_Parrot(t *testing.T) {
	tests := []struct {
		name         string
		ssid         string
		expectMatch  bool
		manufacturer string
		model        string
		isController bool
	}{
		{
			name:         "ANAFI standard",
			ssid:         "ANAFI-A1B2C3",
			expectMatch:  true,
			manufacturer: "Parrot",
			model:        "Anafi",
			isController: false,
		},
		{
			name:         "ANAFI alone",
			ssid:         "ANAFI",
			expectMatch:  true,
			manufacturer: "Parrot",
			model:        "Anafi",
			isController: false,
		},
		{
			name:         "Bebop2",
			ssid:         "Bebop2-123456",
			expectMatch:  true,
			manufacturer: "Parrot",
			model:        "Bebop 2",
			isController: false,
		},
		{
			name:         "BebopDrone",
			ssid:         "BebopDrone-ABC",
			expectMatch:  true,
			manufacturer: "Parrot",
			model:        "Bebop",
			isController: false,
		},
		{
			name:         "Disco with Parrot prefix",
			ssid:         "PARROT-DISCO-FW123",
			expectMatch:  true,
			manufacturer: "Parrot",
			model:        "Disco",
			isController: false,
		},
		{
			name:         "Standalone Disco - no match (too generic)",
			ssid:         "Disco-Fixed-Wing",
			expectMatch:  false,
			manufacturer: "",
			model:        "",
			isController: false,
		},
		{
			name:         "SkyController",
			ssid:         "SkyController3",
			expectMatch:  true,
			manufacturer: "Parrot",
			model:        "",
			isController: true,
		},
		{
			name:         "Parrot generic",
			ssid:         "Parrot-Enterprise",
			expectMatch:  true,
			manufacturer: "Parrot",
			model:        "",
			isController: false,
		},
		{
			name:         "lowercase anafi",
			ssid:         "anafi-usa",
			expectMatch:  true,
			manufacturer: "Parrot",
			model:        "Anafi",
			isController: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := MatchSSID(tc.ssid)

			if tc.expectMatch {
				if result == nil {
					t.Errorf("expected match for SSID %q but got nil", tc.ssid)
					return
				}
				if result.Manufacturer != tc.manufacturer {
					t.Errorf("manufacturer: got %q, want %q", result.Manufacturer, tc.manufacturer)
				}
				if tc.model != "" && result.Model != tc.model {
					t.Errorf("model: got %q, want %q", result.Model, tc.model)
				}
				if result.IsController != tc.isController {
					t.Errorf("isController: got %v, want %v", result.IsController, tc.isController)
				}
			} else {
				if result != nil {
					t.Errorf("expected no match for SSID %q but got %+v", tc.ssid, result)
				}
			}
		})
	}
}

// TestMatchSSID_Autel tests SSID pattern matching for Autel drones
func TestMatchSSID_Autel(t *testing.T) {
	tests := []struct {
		name         string
		ssid         string
		expectMatch  bool
		manufacturer string
		wantModelOrHint string
	}{
		{
			name:         "EVO II",
			ssid:         "EVO-II-PRO",
			expectMatch:  true,
			manufacturer: "Autel",
			wantModelOrHint: "EVO II",
		},
		{
			name:         "EVO III Pro - matches EVO II pattern first",
			ssid:         "EVO-III-PRO-V2",
			expectMatch:  true,
			manufacturer: "Autel",
			wantModelOrHint: "EVO II", // First pattern match wins
		},
		{
			name:         "EVO III Enterprise - matches EVO II pattern first",
			ssid:         "EVO-III-ENT",
			expectMatch:  true,
			manufacturer: "Autel",
			wantModelOrHint: "EVO II", // First pattern match wins
		},
		{
			name:         "EVO Max 4N/4T",
			ssid:         "EVO-MAX-4T",
			expectMatch:  true,
			manufacturer: "Autel",
			wantModelOrHint: "EVO Max",
		},
		{
			name:         "Autel Dragonfish (matches generic pattern first)",
			ssid:         "AUTEL-DRAGONFISH-ENT",
			expectMatch:  true,
			manufacturer: "Autel",
			wantModelOrHint: "generic", // Matches ^Autel[-_ ] pattern first
		},
		{
			name:         "Standalone Dragonfish - no match (too generic)",
			ssid:         "Dragonfish-Enterprise",
			expectMatch:  false,
			manufacturer: "",
			wantModelOrHint: "",
		},
		{
			name:         "Standalone Alpha - no match (removed as too generic)",
			ssid:         "ALPHA-VTOL",
			expectMatch:  false,
			manufacturer: "",
			wantModelOrHint: "",
		},
		{
			name:         "Standalone Titan - no match (removed as too generic)",
			ssid:         "TITAN-Heavy",
			expectMatch:  false,
			manufacturer: "",
			wantModelOrHint: "",
		},
		{
			name:         "Autel generic",
			ssid:         "Autel-X123",
			expectMatch:  true,
			manufacturer: "Autel",
			wantModelOrHint: "generic",
		},
		{
			name:         "default-ssid (broken RemoteID)",
			ssid:         "default-ssid",
			expectMatch:  true,
			manufacturer: "Autel",
			wantModelOrHint: "Autel broken RemoteID",
		},
		{
			name:         "Basic EVO",
			ssid:         "EVO-LITE",
			expectMatch:  true,
			manufacturer: "Autel",
			wantModelOrHint: "EVO",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := MatchSSID(tc.ssid)

			if tc.expectMatch {
				if result == nil {
					t.Errorf("expected match for SSID %q but got nil", tc.ssid)
					return
				}
				if result.Manufacturer != tc.manufacturer {
					t.Errorf("manufacturer: got %q, want %q", result.Manufacturer, tc.manufacturer)
				}
				modelOrHint := result.Model + result.ModelHint
				if !containsIgnoreCase(modelOrHint, tc.wantModelOrHint) {
					t.Errorf("expected Model or ModelHint to contain %q, got Model=%q ModelHint=%q",
						tc.wantModelOrHint, result.Model, result.ModelHint)
				}
			} else {
				if result != nil {
					t.Errorf("expected no match for SSID %q but got %+v", tc.ssid, result)
				}
			}
		})
	}
}

// TestMatchOUI tests OUI lookup accuracy
func TestMatchOUI(t *testing.T) {
	tests := []struct {
		name        string
		mac         string
		expectMatch bool
		contains    string // substring that should be in the description
	}{
		// DJI OUIs
		{
			name:        "DJI 60:60:1F",
			mac:         "60:60:1F:AA:BB:CC",
			expectMatch: true,
			contains:    "DJI",
		},
		{
			name:        "DJI 48:1C:B9",
			mac:         "48:1C:B9:11:22:33",
			expectMatch: true,
			contains:    "DJI",
		},
		{
			name:        "DJI Baiwang",
			mac:         "9C:5A:8A:44:55:66",
			expectMatch: true,
			contains:    "DJI Baiwang",
		},

		// Parrot OUIs
		{
			name:        "Parrot 90:3A:E6",
			mac:         "90:3A:E6:77:88:99",
			expectMatch: true,
			contains:    "Parrot",
		},
		{
			name:        "Parrot controller",
			mac:         "90:03:B7:AA:BB:CC",
			expectMatch: true,
			contains:    "controller",
		},

		// Skydio
		{
			name:        "Skydio 38:1D:14",
			mac:         "38:1D:14:DE:AD:BE",
			expectMatch: true,
			contains:    "Skydio",
		},

		// NOTE: The following OUIs were REMOVED from the verified map to prevent false positives
		// They now correctly return no match - use SSID patterns for these manufacturers instead

		// Autel - IEEE verified OUIs (re-added after WiFi Intel audit)
		{
			name:        "Autel Robotics (IEEE verified)",
			mac:         "EC:5B:CD:11:22:33",
			expectMatch: true,
			contains:    "Autel",
		},
		{
			name:        "Autel Intelligent (IEEE verified)",
			mac:         "18:D7:93:44:55:66",
			expectMatch: true,
			contains:    "Autel",
		},

		// ESP32 - REMOVED as false positive source (generic chip manufacturer)
		{
			name:        "Hubsan (uses Espressif chip)",
			mac:         "24:0A:C4:DE:AD:BE",
			expectMatch: true,
			contains:    "Hubsan",
		},
		{
			name:        "Espressif ESP32 - REMOVED (generic chip, causes FP)",
			mac:         "30:AE:A4:12:34:56",
			expectMatch: false,
			contains:    "",
		},

		// Other manufacturers - re-added after WiFi Intel audit
		{
			name:        "Yuneec (field observed)",
			mac:         "E0:B6:F5:AA:BB:CC",
			expectMatch: true,
			contains:    "Yuneec",
		},
		{
			name:        "Holy Stone (field observed)",
			mac:         "18:C8:E7:11:22:33",
			expectMatch: true,
			contains:    "Holy Stone",
		},
		{
			name:        "Hubsan (field observed)",
			mac:         "98:AA:FC:DE:AD:BE",
			expectMatch: true,
			contains:    "Hubsan",
		},
		{
			name:        "GoPro Karma - REMOVED (not IEEE verified)",
			mac:         "D4:D9:19:12:34:56",
			expectMatch: false,
			contains:    "",
		},
		{
			name:        "Zipline (IEEE verified)",
			mac:         "74:B8:0F:AA:BB:CC",
			expectMatch: true,
			contains:    "Zipline",
		},
		{
			name:        "Goertek/DJI Tello - REMOVED (not IEEE verified)",
			mac:         "08:16:D5:11:22:33",
			expectMatch: false,
			contains:    "",
		},

		// Edge cases
		{
			name:        "Unknown OUI",
			mac:         "00:11:22:33:44:55",
			expectMatch: false,
			contains:    "",
		},
		{
			name:        "Empty MAC",
			mac:         "",
			expectMatch: false,
			contains:    "",
		},
		{
			name:        "Short MAC",
			mac:         "60:60",
			expectMatch: false,
			contains:    "",
		},
		{
			name:        "Dash format - not normalized by MatchOUI",
			mac:         "60-60-1F-AA-BB-CC",
			expectMatch: false, // MatchOUI expects colon format
			contains:    "",
		},
		{
			name:        "Lowercase",
			mac:         "60:60:1f:aa:bb:cc",
			expectMatch: true,
			contains:    "DJI",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := MatchOUI(tc.mac)

			if tc.expectMatch {
				if result == "" {
					t.Errorf("expected match for MAC %q but got empty string", tc.mac)
					return
				}
				if tc.contains != "" && !containsIgnoreCase(result, tc.contains) {
					t.Errorf("OUI result %q should contain %q for MAC %s", result, tc.contains, tc.mac)
				}
			} else {
				if result != "" {
					t.Errorf("expected no match for MAC %q but got %q", tc.mac, result)
				}
			}
		})
	}
}

// TestControllerDetection tests distinguishing controllers from drones
func TestControllerDetection(t *testing.T) {
	tests := []struct {
		name         string
		ssid         string
		isController bool
	}{
		// DJI Controllers
		{"DJI RC", "DJI-RC-PRO", true},
		{"DJI RC N1", "DJI-RC-N1", true},
		{"DJI Goggles 2", "DJIGOGGLES2", true},   // No hyphen after DJI to avoid generic pattern match
		{"DJI Goggles N3", "DJIGOGGLES3", true},  // Pattern is ^DJI[_ ]?GOGGLES[-_ ]?[N23]

		// Parrot Controllers
		{"SkyController", "SkyController3", true},
		{"SkyController with space", "SkyController 3", true},

		// Drones (not controllers)
		{"DJI Mavic", "DJI-MAVIC3", false},
		{"DJI Mini", "DJI_MINI3PRO", false},
		{"DJI Avata", "DJI_AVATA2", false},
		{"Parrot Anafi", "ANAFI-AI", false},
		{"Autel EVO", "EVO-III-PRO", false},
		{"Skydio drone", "SKYDIO-X10D", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := MatchSSID(tc.ssid)
			if result == nil {
				if tc.isController {
					t.Errorf("expected match for controller SSID %q", tc.ssid)
				}
				return
			}
			if result.IsController != tc.isController {
				t.Errorf("SSID %q: got isController=%v, want %v", tc.ssid, result.IsController, tc.isController)
			}
		})
	}
}

// TestDJISerialParsing tests DJI serial number model extraction
// Updated to use real Nzyme field data with CTA-2063-A family mapping (TAP Intel v2.6.0)
func TestDJISerialParsing(t *testing.T) {
	tests := []struct {
		name     string
		serial   string
		expected string
	}{
		// === NZYME FIELD DATA - CTA-2063-A Family Mapping (Feb 2026) ===

		// Mavic 2 family (verified via community data)
		{
			name:     "Mavic 2 Pro (1581F163)",
			serial:   "1581F163CH85R0A30JG0",
			expected: "Mavic 2 Pro",
		},

		// Air 2S (DJI FAQ: 3YT = Air 2S)
		{
			name:     "Air 2S (1581F3YT)",
			serial:   "1581F3YTDJ1V00385UH0",
			expected: "Air 2S",
		},

		// FPV/Avata family (1581F4x) - CTA-2063-A digit "4"
		{
			name:     "Avata (FAA DOC)",
			serial:   "1581F4QWB234200300WQ",
			expected: "Avata",
		},
		{
			name:     "Avata variant",
			serial:   "1581F4QZB21C61BE04MN",
			expected: "Avata",
		},
		{
			name:     "Mavic 3 (1581F45T)",
			serial:   "1581F45T7228200SV14L",
			expected: "Mavic 3",
		},

		// Mavic family (1581F5x) - CTA-2063-A digit "5"
		{
			name:     "Mavic 3 Cine (1581F5BK)",
			serial:   "1581F5BKD224T00B4T8T",
			expected: "Mavic 3 Cine",
		},
		{
			name:     "Mavic 3 Enterprise (1581F5FH)",
			serial:   "1581F5FH7245N002E0HU",
			expected: "Mavic 3 Enterprise",
		},

		// Air family (1581F6x) - CTA-2063-A digit "6"
		{
			name:     "Air 2S (Nzyme confirmed)",
			serial:   "1581F6N8C237H0031Z2M",
			expected: "Air 2S",
		},
		{
			name:     "Air 3S (Nzyme confirmed)",
			serial:   "1581F6QAD244N00C15LS",
			expected: "Air 3S",
		},
		{
			name:     "Mavic 3 Classic (1581F67Q)",
			serial:   "1581F67QE238700A00KR",
			expected: "Mavic 3 Classic",
		},

		// Matrice family (1581F7x) - CTA-2063-A digit "7"
		{
			name:     "Matrice 4T (Nzyme confirmed)",
			serial:   "1581F7FVC251A00CB04F",
			expected: "Matrice 4T",
		},

		// Phantom family (1581F8x) - CTA-2063-A digit "8"
		{
			name:     "Phantom 4 Pro (1581F895)",
			serial:   "1581F895C2563007E969",
			expected: "Phantom 4 Pro",
		},
		{
			name:     "Matrice 30 (1581F8LQ)",
			serial:   "1581F8LQC2532002028U",
			expected: "Matrice 30",
		},

		// Inspire family (1581F9x) - CTA-2063-A digit "9"
		{
			name:     "Inspire 3 (1581F9DE)",
			serial:   "1581F9DEC2594029W68Z",
			expected: "Inspire 3",
		},

		// Edge cases
		{
			name:     "Unknown prefix",
			serial:   "1581XXX1234567890",
			expected: "",
		},
		{
			name:     "Too short",
			serial:   "158",
			expected: "",
		},
		{
			name:     "Empty serial",
			serial:   "",
			expected: "",
		},
		// Generic DJI fallback for unrecognized 1581F prefix
		{
			name:     "Unknown 1581F variant",
			serial:   "1581FXYZ1234567890",
			expected: "DJI (Unknown)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := GetModelFromSerial(tc.serial)
			if result != tc.expected {
				t.Errorf("GetModelFromSerial(%q) = %q, want %q", tc.serial, result, tc.expected)
			}
		})
	}
}

// TestExtractDJIModelFromSSID tests DJI model extraction from SSID
// Note: This function uses substring matching against djiSSIDModels map.
// Since Go maps have random iteration order, tests verify that:
// 1. A relevant model is returned for DJI SSIDs
// 2. Empty string is returned for non-DJI SSIDs
func TestExtractDJIModelFromSSID(t *testing.T) {
	tests := []struct {
		ssid           string
		expectNonEmpty bool      // true if we expect a model to be found
		validOutputs   []string  // acceptable outputs (any of these is OK)
	}{
		// Mavic series - may match MAVIC2 or MAVIC2PRO depending on map order
		{"DJI-MAVICPRO", true, []string{"Mavic Pro"}},
		{"MAVIC2PRO", true, []string{"Mavic 2", "Mavic 2 Pro"}},
		{"MAVIC3", true, []string{"Mavic 3"}},
		{"MAVIC4PRO", true, []string{"Mavic 4", "Mavic 4 Pro"}},

		// Mini series
		{"MINI2-ABC", true, []string{"Mini 2"}},
		{"MINI3PRO", true, []string{"Mini 3", "Mini 3 Pro"}},
		{"MINI4PRO", true, []string{"Mini 4", "Mini 4 Pro"}},

		// Air series
		{"AIR2S", true, []string{"Air 2", "Air 2S"}},
		{"AIR3-DRONE", true, []string{"Air 3"}},

		// FPV/Avata
		{"DJIFPV", true, []string{"FPV"}},
		{"AVATA", true, []string{"Avata"}},
		{"AVATA2", true, []string{"Avata", "Avata 2"}},
		{"NEO-123", true, []string{"Neo"}},

		// Enterprise
		{"MATRICE30T", true, []string{"Matrice 30", "Matrice 30T"}},
		{"INSPIRE2", true, []string{"Inspire 2"}},

		// Agricultural
		{"AGRAST50", true, []string{"Agras T50"}},
		{"AGRAST60", true, []string{"Agras T60"}},
		{"FLYCART30", true, []string{"FlyCart 30"}},

		// Non-DJI patterns - no match
		{"RandomSSID", false, []string{""}},
		{"ANAFI", false, []string{""}},
		{"EVO-III", false, []string{""}},
		{"", false, []string{""}},
	}

	for _, tc := range tests {
		t.Run(tc.ssid, func(t *testing.T) {
			result := ExtractDJIModelFromSSID(tc.ssid)
			if tc.expectNonEmpty {
				if result == "" {
					t.Errorf("ExtractDJIModelFromSSID(%q) = empty, want one of %v", tc.ssid, tc.validOutputs)
				} else {
					found := false
					for _, valid := range tc.validOutputs {
						if result == valid {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("ExtractDJIModelFromSSID(%q) = %q, want one of %v", tc.ssid, result, tc.validOutputs)
					}
				}
			} else if result != "" {
				t.Errorf("ExtractDJIModelFromSSID(%q) = %q, want empty", tc.ssid, result)
			}
		})
	}
}

// TestAnalyzeWiFiFingerprint tests the comprehensive fingerprinting function
func TestAnalyzeWiFiFingerprint(t *testing.T) {
	tests := []struct {
		name           string
		mac            string
		ssid           string
		rssi           float64
		beaconInterval int
		expectResult   bool
		minConfidence  int
		manufacturer   string
		isController   bool
	}{
		{
			name:           "DJI Mavic - full match",
			mac:            "60:60:1F:AA:BB:CC",
			ssid:           "DJI-MAVIC3PRO",
			rssi:           -65,
			beaconInterval: 102,
			expectResult:   true,
			minConfidence:  75,
			manufacturer:   "DJI",
			isController:   false,
		},
		{
			name:           "Parrot Anafi - OUI + SSID",
			mac:            "90:3A:E6:11:22:33",
			ssid:           "ANAFI-AI",
			rssi:           -70,
			beaconInterval: 100,
			expectResult:   true,
			minConfidence:  60,
			manufacturer:   "Parrot",
			isController:   false,
		},
		{
			name:           "OUI only match",
			mac:            "60:60:1F:AA:BB:CC",
			ssid:           "RANDOM-SSID",
			rssi:           -65,
			beaconInterval: 0,
			expectResult:   true,
			minConfidence:  30,
			manufacturer:   "",
			isController:   false,
		},
		{
			name:           "SSID only match",
			mac:            "00:11:22:33:44:55", // Unknown OUI
			ssid:           "DJI-MAVIC3",
			rssi:           -65,
			beaconInterval: 0,
			expectResult:   true,
			minConfidence:  35,
			manufacturer:   "DJI",
			isController:   false,
		},
		{
			name:           "Controller detection",
			mac:            "60:60:1F:AA:BB:CC",
			ssid:           "DJI-RC-PRO",
			rssi:           -50,
			beaconInterval: 100,
			expectResult:   true,
			minConfidence:  50,
			manufacturer:   "DJI",
			isController:   true,
		},
		{
			name:           "No match - random with non-drone beacon interval",
			mac:            "00:11:22:33:44:55",
			ssid:           "MyHomeWifi",
			rssi:           -65,
			beaconInterval: 99, // Not a drone beacon interval (100, 102, 1024)
			expectResult:   false,
			minConfidence:  0,
			manufacturer:   "",
			isController:   false,
		},
		{
			name:           "DJI drone beacon interval boost",
			mac:            "60:60:1F:AA:BB:CC",
			ssid:           "DJI-AIR3",
			rssi:           -60,
			beaconInterval: 102,
			expectResult:   true,
			minConfidence:  80, // OUI + SSID + beacon
			manufacturer:   "DJI",
			isController:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := AnalyzeWiFiFingerprint(tc.mac, tc.ssid, tc.rssi, tc.beaconInterval)

			if tc.expectResult {
				if result == nil {
					t.Errorf("expected result for %s/%s but got nil", tc.mac, tc.ssid)
					return
				}
				if result.Confidence < tc.minConfidence {
					t.Errorf("confidence %d below minimum %d", result.Confidence, tc.minConfidence)
				}
				if tc.manufacturer != "" && result.Manufacturer != tc.manufacturer {
					t.Errorf("manufacturer: got %q, want %q", result.Manufacturer, tc.manufacturer)
				}
				if result.IsController != tc.isController {
					t.Errorf("isController: got %v, want %v", result.IsController, tc.isController)
				}
			} else {
				if result != nil {
					t.Errorf("expected no result but got %+v", result)
				}
			}
		})
	}
}

// TestIsLocallyAdministeredMAC tests LAA detection in intel package
func TestIsLocallyAdministeredMAC(t *testing.T) {
	tests := []struct {
		mac    string
		expect bool
	}{
		{"02:AA:BB:CC:DD:EE", true},
		{"06:AA:BB:CC:DD:EE", true},
		{"0A:AA:BB:CC:DD:EE", true},
		{"0E:AA:BB:CC:DD:EE", true},
		{"00:AA:BB:CC:DD:EE", false},
		{"60:60:1F:AA:BB:CC", false},
		{"", false},
		{"X", false},
	}

	for _, tc := range tests {
		t.Run(tc.mac, func(t *testing.T) {
			got := IsLocallyAdministeredMAC(tc.mac)
			if got != tc.expect {
				t.Errorf("IsLocallyAdministeredMAC(%s) = %v, want %v", tc.mac, got, tc.expect)
			}
		})
	}
}

// TestGetKnownOUIs tests that OUI map is properly populated with IEEE-verified OUIs only
func TestGetKnownOUIs(t *testing.T) {
	ouis := GetKnownOUIs()

	// Check IEEE-verified OUIs (DJI, Parrot, Skydio only - others removed for false positive prevention)
	expectedOUIs := []string{
		"60:60:1F", // DJI - IEEE verified
		"34:D2:62", // DJI - IEEE verified
		"90:3A:E6", // Parrot - IEEE verified
		"38:1D:14", // Skydio - IEEE verified
	}

	for _, oui := range expectedOUIs {
		if _, ok := ouis[oui]; !ok {
			t.Errorf("expected IEEE-verified OUI %q not found in map", oui)
		}
	}

	// Verify OUIs that should NOT be present (generic chip manufacturers)
	removedOUIs := []string{
		"00:E0:4C", // Realtek - generic chip manufacturer
	}

	for _, oui := range removedOUIs {
		if _, ok := ouis[oui]; ok {
			t.Errorf("OUI %q should be removed (false positive risk)", oui)
		}
	}

	// Verify minimum count (expanded after WiFi Intel audit unified TAP + Node OUIs)
	if len(ouis) < 25 {
		t.Errorf("expected at least 25 OUIs after unification, got %d", len(ouis))
	}
}

// TestOtherManufacturers tests SSID patterns for other manufacturers
// NOTE: Standalone generic patterns (ZINO, X8SE, Cetus, Typhoon, etc.) were REMOVED
// to prevent false positives. Now require manufacturer prefix.
func TestOtherManufacturers(t *testing.T) {
	tests := []struct {
		ssid         string
		manufacturer string
		model        string
		shouldMatch  bool
	}{
		// Skydio - require SKYDIO prefix
		{"SKYDIO-X10D", "Skydio", "", true},
		{"Skydio-S2", "Skydio", "", true},
		{"S2-PLUS", "", "", false}, // REMOVED - too generic

		// Yuneec - require YUNEEC prefix
		{"Yuneec-Typhoon", "Yuneec", "", true},
		{"Typhoon-H520", "", "", false}, // REMOVED - too generic
		{"Mantis-Q", "", "", false},     // REMOVED - too generic

		// Holy Stone - require HolyStone prefix
		{"HolyStone-HS720", "Holy Stone", "", true},
		{"HS720-Drone", "", "", false}, // REMOVED - matches TP-Link smart plugs

		// Hubsan - require HUBSAN prefix
		{"Hubsan-Zino", "Hubsan", "", true},
		{"ZINO-MINI", "", "", false}, // REMOVED - too generic

		// FIMI - require FIMI prefix
		{"FIMI-X8SE", "FIMI", "", true},
		{"X8SE-PRO", "", "", false}, // REMOVED - too generic

		// Zero Zero Robotics - specific enough
		{"HOVERAir-X1", "Zero Zero Robotics", "HOVERAir X1", true},
		{"HOVER-AIR", "Zero Zero Robotics", "HOVERAir X1", true},

		// BetaFPV - require BETAFPV prefix
		{"BETAFPV-Cetus", "BetaFPV", "", true},
		{"Cetus-Pro", "", "", false}, // REMOVED - too generic

		// Enterprise
		{"Wingtra-Survey", "Wingtra", "", true},
		{"senseFly-eBee", "AgEagle (senseFly)", "", true},
		{"eBee-X", "AgEagle (senseFly)", "", true},
		{"Elios-3", "Flyability", "", true},
		{"Brinc-Lemur", "Brinc", "", true},
		{"Lemur-Tactical", "Brinc", "", true},

		// RemoteID beacons - always specific enough
		{"RID-Beacon", "RemoteID", "", true},
		{"DroneID-001", "OpenDroneID", "", true},
		{"DroneBeacon-100", "BlueMark", "", true},
		{"Dronetag-XY", "Dronetag", "", true},

		// Generic WiFi FPV
		{"FPV-WIFI-DRONE", "Generic", "", true},
		{"WiFi-FPV-Cam", "Generic", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.ssid, func(t *testing.T) {
			result := MatchSSID(tc.ssid)
			if tc.shouldMatch {
				if result == nil {
					t.Errorf("expected match for SSID %q", tc.ssid)
					return
				}
				if result.Manufacturer != tc.manufacturer {
					t.Errorf("manufacturer: got %q, want %q", result.Manufacturer, tc.manufacturer)
				}
				if tc.model != "" && result.Model != tc.model {
					t.Errorf("model: got %q, want %q", result.Model, tc.model)
				}
			} else {
				if result != nil {
					t.Errorf("expected NO match for SSID %q (pattern removed as too generic)", tc.ssid)
				}
			}
		})
	}
}

// Helper function for case-insensitive contains
func containsIgnoreCase(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
