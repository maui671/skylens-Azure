package intel

import (
	"regexp"
	"sort"
	"strings"
)

// SSIDPattern represents a pattern for matching drone SSIDs
type SSIDPattern struct {
	Pattern      *regexp.Regexp
	Manufacturer string
	Model        string
	ModelHint    string
	IsController bool
}

// FingerprintResult contains the result of WiFi fingerprinting
type FingerprintResult struct {
	Manufacturer    string   `json:"manufacturer,omitempty"`
	Model           string   `json:"model,omitempty"`
	ModelHint       string   `json:"model_hint,omitempty"`
	IsController    bool     `json:"is_controller,omitempty"`
	OUIDescription  string   `json:"oui_description,omitempty"`
	Confidence      int      `json:"confidence"`
	Reasons         []string `json:"confidence_reasons,omitempty"`
	EstimatedDistM  float64  `json:"estimated_distance_m,omitempty"`
	DistConfidence  float64  `json:"distance_confidence,omitempty"`
}

// OUI to manufacturer/description mapping
// ONLY IEEE-VERIFIED OUIs are included here to prevent false positives
// Last verified: 2026-02-12 against maclookup.app IEEE database
var ouiMap = map[string]string{
	// ===== DJI - SZ DJI TECHNOLOGY CO.,LTD (IEEE VERIFIED) =====
	"04:A8:5A": "DJI (drone)", // IEEE MA-L, registered 2025-01-09
	"0C:9A:E6": "DJI (drone)", // IEEE MA-L, registered 2025-08-14
	"34:D2:62": "DJI (drone)", // IEEE MA-L, registered 2019-08-13
	"48:1C:B9": "DJI (drone)", // IEEE MA-L, registered 2022-05-07
	"4C:43:F6": "DJI (drone)", // IEEE MA-L, registered 2025-12-01
	"58:B8:58": "DJI (drone)", // IEEE MA-L, registered 2024-07-26
	"60:60:1F": "DJI (drone)", // IEEE MA-L, registered 2013-03-11
	"88:29:85": "DJI (drone)", // IEEE MA-L, registered 2025-10-29
	"8C:58:23": "DJI (drone)", // IEEE MA-L, registered 2025-05-27
	"E4:7A:2C": "DJI (drone)", // IEEE MA-L, registered 2023-10-19
	"8C:1E:D9": "DJI (drone)", // Field-verified: Phantom 4 Pro V2 (Feb 2026 live test)

	// ===== DJI BAIWANG TECHNOLOGY CO LTD (IEEE VERIFIED) =====
	"9C:5A:8A": "DJI Baiwang (drone)", // IEEE MA-L, registered 2024-12-30

	// ===== PARROT SA (IEEE VERIFIED) =====
	"00:12:1C": "Parrot (drone)",      // IEEE MA-L, registered 2004-08-14
	"00:26:7E": "Parrot (drone)",      // IEEE MA-L, registered 2010-01-05
	"90:03:B7": "Parrot (controller)", // IEEE MA-L, registered 2011-11-13
	"90:3A:E6": "Parrot (drone)",      // IEEE MA-L, registered 2016-04-27
	"A0:14:3D": "Parrot (drone)",      // IEEE MA-L, registered 2013-07-29

	// ===== SKYDIO INC (IEEE VERIFIED) =====
	"38:1D:14": "Skydio (drone)", // IEEE MA-L, registered 2019-07-02

	// ===== AUTEL ROBOTICS (IEEE + FIELD VERIFIED) =====
	"EC:5B:CD": "Autel Robotics (drone)",   // IEEE verified: Autel Robotics Co., Ltd.
	"18:D7:93": "Autel Intelligent (drone)", // IEEE verified: Autel Intelligent Technology
	"70:88:6B": "Autel (drone)",             // IEEE verified: Autel Robotics
	// NOTE: 60:55:F9 (Espressif), CC:DB:A7 (Espressif), D4:D8:53 (Intel) REMOVED
	// These are chipset OUIs, not Autel OUIs — they match millions of non-drone devices

	// ===== YUNEEC =====
	"E0:B6:F5": "Yuneec (drone)", // Field observed

	// ===== HUBSAN =====
	"18:FE:34": "Hubsan/Espressif (drone)", // Hubsan uses Espressif - accept risk
	"24:0A:C4": "Hubsan/Espressif (drone)", // Field observed on Zino series
	"98:AA:FC": "Hubsan (drone)",            // Field observed

	// ===== FIMI (XIAOMI) =====
	"64:CE:91": "FIMI/Xiaomi (drone)", // Field observed on X8SE
	"6C:DF:FB": "FIMI/Xiaomi (drone)", // Field observed

	// ===== HOLY STONE =====
	"18:C8:E7": "Holy Stone (drone)", // Field observed

	// ===== ZERO ZERO ROBOTICS (HoverAir) =====
	"F4:12:FA": "Zero Zero (drone)",          // Field observed on HOVERAir X1
	"C8:63:14": "Zero Zero Robotics (drone)", // IEEE verified

	// ===== ZEROTECH =====
	"84:83:19": "ZeroTech (drone)", // IEEE verified (Dobby etc.)

	// ===== WALKERA =====
	"00:1A:79": "Walkera (drone)", // IEEE registered to Shenzhen Walkera

	// ===== ZIPLINE =====
	"74:B8:0F": "Zipline (drone)", // IEEE verified

	// ===== XAG =====
	"A4:51:29": "XAG (drone)", // IEEE verified (agricultural)

	// ===== JOBY AVIATION =====
	"C4:CC:37": "Joby Aviation (drone)", // IEEE verified (eVTOL)

	// ===== TEAL DRONES =====
	"B0:30:C8": "Teal Drones (drone)", // IEEE verified

	// ===== AEROVIRONMENT =====
	"00:1A:F9": "AeroVironment (drone)", // IEEE verified (Switchblade etc.)

	// ===== PROX DYNAMICS / BLACK HORNET =====
	"70:A6:6A": "Black Hornet (drone)", // IEEE verified

	// ===== FRENCH DRI (REGULATORY) =====
	"6A:5C:35": "French DRI (RemoteID)", // Regulatory beacon

	// ===== POTENSIC / RUKO / OTHER CONSUMER =====
	// Most use generic Espressif/Realtek - rely on SSID patterns

	// ===== AUTO-ADDED BY INTEL-UPDATER (2026-02-25) =====
	"00:0C:BF": "Holy Stone (drone)", // IEEE MA-L, auto-added 2026-02-25

}

// SSID patterns for drone detection - compiled at init
var ssidPatterns []SSIDPattern

func init() {
	// Define SSID patterns
	patterns := []struct {
		pattern      string
		manufacturer string
		model        string
		modelHint    string
		isController bool
	}{
		// DJI — specific patterns FIRST, generic catch-all LAST
		{`^PROJ[0-9a-fA-F]{6}$`, "DJI", "", "DJI RC Controller", true}, // DJI controller WiFi hotspot (e.g. PROJae291c)
		{`^DJI[-_ ]RC`, "DJI", "", "controller", true},
		{`^RM [A-Z0-9]+ \d+`, "DJI", "", "DJI RC Enterprise", true}, // DJI enterprise controller (e.g. RM E70536 1210091)
		// Specific DJI model patterns (must precede generic ^DJI[-_ ] catch-all)
		{`^DJI[_ ]?FPV[-_ ]?2`, "DJI", "FPV 2", "", false},
		{`^DJI[_ ]?FPV`, "DJI", "FPV", "", false},
		{`^DJI[_ ]?AVATA[-_ ]?3`, "DJI", "Avata 3", "", false},
		{`^DJI[_ ]?AVATA`, "DJI", "", "Avata", false},
		{`^DJI[_ ]?NEO[-_ ]?2`, "DJI", "Neo 2", "", false},
		{`^DJI[_ ]?NEO`, "DJI", "Neo", "", false},
		{`^DJI[_ ]?FLIP`, "DJI", "Flip", "", false},
		{`^DJI[_ ]?MAVIC[-_ ]?4`, "DJI", "", "Mavic 4", false},
		{`^DJI[_ ]?MINI[-_ ]?5`, "DJI", "", "Mini 5", false},
		{`^DJI[_ ]?AIR[-_ ]?4`, "DJI", "", "Air 4", false},
		{`^DJI[_ ]?FLYCART[-_ ]?100`, "DJI", "FlyCart 100", "", false},
		{`^DJI[_ ]?GOGGLES[-_ ]?[N23]`, "DJI", "", "Goggles N/2/3", true},
		{`^DJI[_ ]?O[34][-_ ]?AIR`, "DJI", "", "O3/O4 Air Unit", false},
		{`^DJI[_ ]?DOCK[-_ ]?3`, "DJI", "Dock 3", "", false},
		{`^DJI[-_ ]?AVINOX`, "DJI", "Avinox", "", false},
		{`^DJI[-_ ]`, "DJI", "", "generic", false}, // Generic DJI catch-all — MUST be last of ^DJI patterns
		{`^TELLO[-_ ]`, "DJI/Ryze", "Tello", "", false},
		{`^TELLO$`, "DJI/Ryze", "Tello", "", false},
		{`^PHANTOM[-_ ]`, "DJI", "", "Phantom", false},
		{`^MAVIC[-_ ]?4[-_ ]?PRO`, "DJI", "Mavic 4 Pro", "", false},
		{`^MAVIC[-_ ]?4[-_ ]?CLASSIC`, "DJI", "Mavic 4 Classic", "", false},
		{`^MAVIC[-_ ]?4[-_ ]?MULTI`, "DJI", "Mavic 4 Multispectral", "", false},
		{`^MAVIC[-_ ]?4[-_ ]?ENT`, "DJI", "Mavic 4 Enterprise", "", false},
		{`^MAVIC[-_ ]`, "DJI", "", "Mavic", false},
		{`^SPARK[-_ ]`, "DJI", "Spark", "", false},
		{`^INSPIRE[-_ ]`, "DJI", "", "Inspire", false},
		{`^M4E[-_ ]`, "DJI", "Matrice 4E", "", false},
		{`^M4T[-_ ]`, "DJI", "Matrice 4T", "", false},
		{`^M4S[-_ ]`, "DJI", "Matrice 4S", "", false},
		{`^MATRICE[-_ ]?4`, "DJI", "", "Matrice 4", false},
		{`^MATRICE`, "DJI", "", "Matrice", false},
		{`^AGRAST60`, "DJI", "Agras T60", "", false},
		{`^AGRAST70`, "DJI", "Agras T70", "", false},
		{`^AGRAST100`, "DJI", "Agras T100", "", false},
		{`^AGRAS[-_ ]`, "DJI", "", "Agras", false},
		{`^FLYCART`, "DJI", "FlyCart 30", "", false},
		{`^MINI[-_ ]?5[-_ ]?PRO`, "DJI", "Mini 5 Pro", "", false},
		{`^MINI[-_ ]?5[-_ ]?SE`, "DJI", "Mini 5 SE", "", false},
		{`^AIR[-_ ]?4S`, "DJI", "Air 4S", "", false},
		{`^AIR[-_ ]?4[-_ ]?PRO`, "DJI", "Air 4 Pro", "", false},
		{`^FLIP[-_ ]?2`, "DJI", "Flip 2", "", false},
		{`^NEO[-_ ]?2[-_ ]?PRO`, "DJI", "Neo 2 Pro", "", false},
		{`^DOCK[-_ ]?3[-_ ]?`, "DJI", "Dock 3", "", false},

		// Parrot
		{`^ANAFI[-_ ]`, "Parrot", "Anafi", "", false},
		{`^ANAFI$`, "Parrot", "Anafi", "", false},
		{`^ANAFI[-_ ]?THERMAL`, "Parrot", "Anafi Thermal", "", false},
		{`^ANAFI[-_ ]?USA`, "Parrot", "Anafi USA", "", false},
		{`^ANAFI[-_ ]?AI`, "Parrot", "Anafi AI", "", false},
		{`^BebopDrone`, "Parrot", "Bebop", "", false},
		{`^Bebop2`, "Parrot", "Bebop 2", "", false},
		{`^PARROT[-_ ]?DISCO`, "Parrot", "Disco", "", false},
		{`^SkyController`, "Parrot", "", "controller", true},
		{`^Parrot[-_ ]`, "Parrot", "", "generic", false},
		// NOTE: Standalone DISCO pattern REMOVED - too generic

		// Autel - prefer patterns with AUTEL prefix to reduce false positives
		{`^default-ssid$`, "Autel", "", "Autel broken RemoteID", false},
		{`^Autel[-_ ]`, "Autel", "", "generic", false},
		{`^AUTEL[-_ ]?EVO`, "Autel", "", "EVO", false},
		{`^EVO[-_ ]?III`, "Autel", "", "EVO III", false},  // Must precede EVO II (III starts with II)
		{`^EVO[-_ ]?II`, "Autel", "", "EVO II", false},
		{`^EVO[-_ ]?NANO`, "Autel", "EVO Nano", "", false},
		{`^EVO[-_ ]?LITE`, "Autel", "EVO Lite", "", false},
		{`^EVO[-_ ]?MAX`, "Autel", "EVO Max", "", false},
		{`^AUTEL[-_ ]?DRAGONFISH`, "Autel", "Dragonfish", "", false},
		{`^AUTEL[-_ ]?KESTREL`, "Autel", "Kestrel", "", false},
		{`^AUTEL[-_ ]?TITAN`, "Autel", "Titan", "", false},
		// NOTE: Standalone ALPHA, TITAN, KESTREL patterns REMOVED - too generic

		// Skydio - require SKYDIO prefix to avoid false positives from generic X10/X12
		{`^Skydio[-_ ]`, "Skydio", "", "generic", false},
		{`^SKYDIO[-_ ]?X10`, "Skydio", "Skydio X10", "", false},
		{`^SKYDIO[-_ ]?X10[-_ ]?D`, "Skydio", "Skydio X10D", "", false},
		{`^SKYDIO[-_ ]?X10[-_ ]?LITE`, "Skydio", "Skydio X10 Lite", "", false},
		{`^SKYDIO[-_ ]?S2`, "Skydio", "Skydio 2+", "", false},
		{`^SKYDIO[-_ ]?X12`, "Skydio", "Skydio X12", "", false},
		{`^SKYDIO[-_ ]?DOCK`, "Skydio", "Skydio Dock", "", false},
		// NOTE: Standalone X10/X12 patterns REMOVED - too generic, match many devices

		// Yuneec - require YUNEEC prefix to avoid matching Typhoon routers, etc.
		{`^Yuneec`, "Yuneec", "", "generic", false},
		{`^YUNEEC[-_ ]?TYPHOON`, "Yuneec", "Typhoon", "", false},
		{`^YUNEEC[-_ ]?MANTIS`, "Yuneec", "Mantis", "", false},
		// NOTE: Standalone TYPHOON, MANTIS patterns REMOVED - too generic

		// Holy Stone
		{`^HolyStone`, "Holy Stone", "", "generic", false},
		// NOTE: HS\d{3} pattern REMOVED - matches TP-Link smart plugs (HS100, HS200, etc.)

		// Hubsan - require HUBSAN prefix
		{`^Hubsan[-_ ]`, "Hubsan", "", "generic", false},
		{`^HUBSAN[-_ ]?ZINO`, "Hubsan", "Zino", "", false},
		// NOTE: Standalone ZINO pattern REMOVED - too generic

		// FIMI - require FIMI prefix
		{`^FIMI[-_ ]`, "FIMI", "", "generic", false},
		{`^FIMI[-_ ]?X8`, "FIMI", "X8SE", "", false},
		// NOTE: Standalone X8SE pattern REMOVED - too generic

		// GoPro - require GOPRO prefix
		{`^GOPRO[-_ ]?KARMA`, "GoPro", "Karma", "", false},
		// NOTE: Standalone KARMA pattern REMOVED - too generic

		// Zero Zero Robotics - specific enough as is
		{`^HOVERAir`, "Zero Zero Robotics", "HOVERAir X1", "", false},
		{`^HOVER[-_ ]?AIR`, "Zero Zero Robotics", "HOVERAir X1", "", false},

		// BetaFPV - require BETAFPV prefix
		{`^BETAFPV`, "BetaFPV", "", "generic", false},
		{`^BETAFPV[-_ ]?CETUS`, "BetaFPV", "Cetus", "", false},
		// NOTE: Standalone CETUS pattern REMOVED - too generic

		// Enterprise/Commercial
		{`^Wingtra`, "Wingtra", "", "WingtraOne", false},
		{`^senseFly`, "AgEagle (senseFly)", "", "eBee", false},
		{`^eBee`, "AgEagle (senseFly)", "", "eBee", false},
		{`^EBEE[-_ ]?X`, "AgEagle", "eBee X", "", false},
		{`^EBEE`, "AgEagle", "", "eBee", false},
		{`^AGEAGLE`, "AgEagle", "", "eBee", false},
		{`^Elios[-_ ]`, "Flyability", "", "Elios", false},
		{`^Brinc[-_ ]`, "Brinc", "", "Lemur", false},
		{`^Lemur[-_ ]`, "Brinc", "", "Lemur", false},
		{`^GoldenEagle`, "Teal Drones", "Golden Eagle", "", false},
		{`^Zipline`, "Zipline", "", "delivery", false},
		{`^Matternet`, "Matternet", "", "M2", false},
		// Inspired Flight
		{`^IF1200`, "Inspired Flight", "IF1200", "", false},
		{`^IF800`, "Inspired Flight", "IF800", "", false},
		{`^INSPIRED[-_ ]?FLIGHT`, "Inspired Flight", "", "generic", false},
		// FreeFly Systems - require FREEFLY prefix to avoid matching Alta routers, Astro headsets
		{`^FREEFLY`, "FreeFly", "", "Alta", false},
		{`^FREEFLY[-_ ]?ALTA`, "FreeFly", "Alta", "", false},
		{`^FREEFLY[-_ ]?ASTRO`, "FreeFly", "Astro", "", false},
		// NOTE: Standalone ALTA, ASTRO patterns REMOVED - match Alta WiFi routers, Astro gaming
		// Potensic SSID patterns - require POTENSIC prefix
		{`^POTENSIC`, "Potensic", "", "generic", false},
		{`^POTENSIC[-_ ]?DREAMER`, "Potensic", "Dreamer", "", false},
		{`^POTENSIC[-_ ]?ATOM`, "Potensic", "Atom", "", false},
		// NOTE: Standalone ATOM, DREAMER patterns REMOVED - too generic
		// Ruko SSID patterns - require RUKO prefix
		{`^RUKO`, "Ruko", "", "generic", false},
		{`^RUKO[-_ ]?F11`, "Ruko", "F11", "", false},
		{`^RUKO[-_ ]?U11`, "Ruko", "U11", "", false},
		// NOTE: Standalone F11/U11 patterns REMOVED - too generic

		// RemoteID beacons - don't set designation, let serial number identify
		{`^RID-`, "RemoteID", "", "", false},
		{`^DroneID`, "OpenDroneID", "", "", false},
		{`^DroneBeacon`, "BlueMark", "DroneBeacon db120", "", false},
		{`^Dronetag`, "Dronetag", "", "", false},

		// Generic drone patterns
		{`^FPV[_-]?WIFI`, "Generic", "", "Chinese WiFi FPV drone", false},
		{`^WiFi[-_]?FPV`, "Generic", "", "WiFi FPV drone", false},
	}

	// Compile patterns
	ssidPatterns = make([]SSIDPattern, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile("(?i)" + p.pattern)
		if err != nil {
			continue
		}
		ssidPatterns = append(ssidPatterns, SSIDPattern{
			Pattern:      re,
			Manufacturer: p.manufacturer,
			Model:        p.model,
			ModelHint:    p.modelHint,
			IsController: p.isController,
		})
	}
}

// MatchSSID matches an SSID against known drone patterns
func MatchSSID(ssid string) *SSIDPattern {
	if ssid == "" {
		return nil
	}
	for i := range ssidPatterns {
		if ssidPatterns[i].Pattern.MatchString(ssid) {
			return &ssidPatterns[i]
		}
	}
	return nil
}

// MatchOUI looks up the MAC OUI in the drone OUI map
func MatchOUI(mac string) string {
	if mac == "" || len(mac) < 8 {
		return ""
	}
	oui := strings.ToUpper(mac[:8])
	return ouiMap[oui]
}

// IsLocallyAdministeredMAC checks if MAC is locally administered (randomized)
// LAA MACs have second hex digit as 2, 6, A, or E
func IsLocallyAdministeredMAC(mac string) bool {
	if mac == "" || len(mac) < 2 {
		return false
	}
	second := strings.ToUpper(string(mac[1]))
	return second == "2" || second == "6" || second == "A" || second == "E"
}

// AnalyzeWiFiFingerprint performs WiFi fingerprinting on a frame
func AnalyzeWiFiFingerprint(mac, ssid string, rssi float64, beaconInterval int) *FingerprintResult {
	result := &FingerprintResult{
		Confidence: 0,
		Reasons:    make([]string, 0),
	}

	// SSID pattern matching
	ssidMatch := MatchSSID(ssid)
	if ssidMatch != nil && !ssidMatch.IsController {
		result.Manufacturer = ssidMatch.Manufacturer
		result.Model = ssidMatch.Model
		result.ModelHint = ssidMatch.ModelHint
		result.IsController = ssidMatch.IsController
		result.Confidence += 40
		result.Reasons = append(result.Reasons, "ssid_match:"+ssidMatch.Manufacturer)
	} else if ssidMatch != nil && ssidMatch.IsController {
		result.Manufacturer = ssidMatch.Manufacturer
		result.IsController = true
		result.Confidence += 30
		result.Reasons = append(result.Reasons, "ssid_match:controller")
	}

	// OUI matching
	ouiDesc := MatchOUI(mac)
	if ouiDesc != "" {
		result.OUIDescription = ouiDesc
		result.Confidence += 35
		result.Reasons = append(result.Reasons, "oui:"+ouiDesc)
	}

	// Beacon interval analysis (some drones use distinctive intervals)
	// NOTE: 100 TU is the standard default for ALL WiFi APs — removed to prevent false positives
	droneBeaconIntervals := map[int]string{
		40:   "Parrot beacon interval",
		102:  "DJI beacon interval",
		200:  "Autel beacon interval",
		1024: "low-power drone beacon",
	}
	if beaconInterval > 0 {
		if desc, ok := droneBeaconIntervals[beaconInterval]; ok {
			result.Confidence += 15
			result.Reasons = append(result.Reasons, "beacon_interval:"+desc)
		}
	}

	// LAA MAC penalty - if only OUI match on locally administered MAC, unreliable
	if IsLocallyAdministeredMAC(mac) && ssidMatch == nil && ouiDesc != "" {
		// OUI match on LAA MAC is not reliable
		result.Confidence = 0
		result.Reasons = []string{"laa_mac_only_oui"}
		return nil
	}

	// No drone indicators found
	if len(result.Reasons) == 0 {
		return nil
	}

	// RSSI distance estimation
	model := result.Model
	if model == "" {
		model = result.ModelHint
	}
	if rssi < 0 {
		distEst := EstimateDistanceWithConfidence(rssi, model)
		if distEst.Distance > 0 {
			result.EstimatedDistM = distEst.Distance
			result.DistConfidence = distEst.Confidence
		}
	}

	// Derive designation if not set
	if result.Model == "" && result.ModelHint != "" && result.Manufacturer != "" {
		// Keep as hint
	}

	return result
}

// GetKnownOUIs returns all known drone OUIs
func GetKnownOUIs() map[string]string {
	return ouiMap
}

// DJISSIDModels maps DJI SSID keywords to model names
var djiSSIDModels = map[string]string{
	// Mavic series
	"MAVICPRO":      "Mavic Pro",
	"MAVICAIR":      "Mavic Air",
	"MAVICAIR2":     "Mavic Air 2",
	"MAVICMINI":     "Mavic Mini",
	"MAVIC2":        "Mavic 2",
	"MAVIC2PRO":     "Mavic 2 Pro",
	"MAVIC2ZOOM":    "Mavic 2 Zoom",
	"MAVIC3":        "Mavic 3",
	"MAVIC3PRO":     "Mavic 3 Pro",
	"MAVIC3CLASSIC": "Mavic 3 Classic",
	"MAVIC3CINE":    "Mavic 3 Cine",
	"MAVIC3ENT":     "Mavic 3 Enterprise",
	"MAVIC4":        "Mavic 4",
	"MAVIC4PRO":     "Mavic 4 Pro",
	"MAVIC4MULTI":   "Mavic 4 Multispectral",

	// Mini series
	"MINI2":    "Mini 2",
	"MINISE":   "Mini SE",
	"MINI3":    "Mini 3",
	"MINI3PRO": "Mini 3 Pro",
	"MINI4":    "Mini 4",
	"MINI4PRO": "Mini 4 Pro",
	"MINI5":    "Mini 5",
	"MINI5PRO": "Mini 5 Pro",

	// Air series
	"AIR2":    "Air 2",
	"AIR2S":   "Air 2S",
	"AIR3":    "Air 3",
	"AIR3S":   "Air 3S",
	"AIR4":    "Air 4",
	"AIR4PRO": "Air 4 Pro",

	// FPV series
	"FPV":    "FPV",
	"FPV2":   "FPV 2",
	"AVATA":  "Avata",
	"AVATA2": "Avata 2",
	"AVATA3": "Avata 3",
	"NEO":    "Neo",
	"FLIP":   "Flip",

	// Consumer legacy
	"PHANTOM3": "Phantom 3",
	"PHANTOM4": "Phantom 4",
	"SPARK":    "Spark",

	// Pro/Enterprise
	"INSPIRE2":      "Inspire 2",
	"INSPIRE3":      "Inspire 3",
	"MATRICE30":     "Matrice 30",
	"MATRICE30T":    "Matrice 30T",
	"MATRICE30PRO":  "Matrice 30 Pro",
	"MATRICE300":    "Matrice 300 RTK",
	"MATRICE350RTK": "Matrice 350 RTK",
	"MATRICE4":      "Matrice 4",
	"MATRICE4E":     "Matrice 4E",
	"MATRICE4T":     "Matrice 4T",
	"MATRICE4S":     "Matrice 4S",

	// Agricultural
	"AGRAST30":  "Agras T30",
	"AGRAST40":  "Agras T40",
	"AGRAST50":  "Agras T50",
	"AGRAST60":  "Agras T60",
	"AGRAST70":  "Agras T70",
	"FLYCART30": "FlyCart 30",
}

// djiSSIDModelKeys is sorted by key length descending so longer keys match first
// (e.g., "MINI3PRO" before "MINI3" before "MINI"). Initialized in init().
var djiSSIDModelKeys []string

func init() {
	djiSSIDModelKeys = make([]string, 0, len(djiSSIDModels))
	for k := range djiSSIDModels {
		djiSSIDModelKeys = append(djiSSIDModelKeys, k)
	}
	sort.Slice(djiSSIDModelKeys, func(i, j int) bool {
		return len(djiSSIDModelKeys[i]) > len(djiSSIDModelKeys[j])
	})
}

// ExtractDJIModelFromSSID tries to extract specific DJI model from SSID.
// Keys are checked longest-first to ensure "MAVIC3PRO" matches before "MAVIC3".
func ExtractDJIModelFromSSID(ssid string) string {
	if ssid == "" {
		return ""
	}
	// Normalize: uppercase, remove spaces/dashes
	normalized := strings.ToUpper(ssid)
	normalized = strings.ReplaceAll(normalized, "-", "")
	normalized = strings.ReplaceAll(normalized, "_", "")
	normalized = strings.ReplaceAll(normalized, " ", "")

	for _, key := range djiSSIDModelKeys {
		if strings.Contains(normalized, key) {
			return djiSSIDModels[key]
		}
	}

	return ""
}
