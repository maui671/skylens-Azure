// Package intel provides intelligence features for drone detection
// including RSSI-based distance estimation and model identification.
package intel

import (
	"math"
	"strings"
	"sync"
)

// RSSI Calibration Constants
// Based on Log-Distance Path Loss Model:
//   RSSI = RSSI_0 - 10 * n * log10(d)
//   d = 10 ^ ((RSSI_0 - RSSI) / (10 * n))
//
// === SKYLENS FIELD CALIBRATION (Feb 2026, 40 ground-truth GPS RemoteID points) ===
//
// TAPs: RTL8814AU (tap-001), RTL8812AU (tap-002), MT7921U (tap-003)
// Environment: Open field, elevated sensors, channel 6 NAN RemoteID
//
// Ground-truth calibration points (GPS RemoteID vs TAP coordinates):
//   | Distance (km) | RSSI (dBm) | Model          | Points | Notes               |
//   |---------------|------------|----------------|--------|---------------------|
//   |   3.0 - 4.0   | -69 to -78 | Mavic 3 Cine   |   34   | Baseline reference  |
//   |   6.3          |   -79      | Air 3S         |    3   | Consistent readings |
//   |   8.7          |   -89      | Mavic 2 Pro    |    1   | OcuSync 2.0         |
//   |  12.1          |   -95      | Air 2S         |    1   | Near noise floor    |
//   |  36.4          |   -94      | Inspire 3      |    1   | Exceptional range   |
//
// Fitted model (linear regression, 40 points):
//   Path loss exponent n = 2.6  (was 1.8 from Nzyme hardware)
//   BaseRSSI0 = +18.4 dB       (was -26.8 from Nzyme hardware)
//   Residual errors: 0-5% (was 60-87% with old Nzyme calibration)
//
// Why the difference from Nzyme: different TAP hardware/antennas.
// Nzyme data was calibrated for their receivers, not ours.
//
// === LEGACY NZYME DATA (Jan-Feb 2026, 19 drones, Puerto Rico) ===
// Retained for reference only — not used for current calibration.
//   - 19 calibration points, 0.8-14.8 km range
//   - Path loss exponent was ~1.8 (correct for Nzyme hardware)
//   - Detection range up to 14.8 km at -88 dBm

const (
	// BaseRSSI0 is the log-distance model reference parameter (Mavic 3 Cine baseline).
	// Calibrated from 40 ground-truth GPS RemoteID points at n=2.6.
	// Mavic 3 Cine: -69 to -78 dBm @ 3-4km (34 data points, best-fit baseline).
	// IMPORTANT: BaseRSSI0 and DefaultPathLossN are coupled parameters.
	// If you change n, you MUST recompute BaseRSSI0 from the same calibration data.
	BaseRSSI0 = 18.4

	// GenericRSSI0 is conservative fallback for unknown models.
	// Slightly lower than BaseRSSI0 so unknown drones are estimated
	// closer (conservative for security — prefer false-close over false-far).
	GenericRSSI0 = 17.0

	// DefaultPathLossN is the path loss exponent for open-field elevated sensor.
	// Calibrated from Skylens field data (40 ground-truth GPS points, 3-36km,
	// RTL8814AU/RTL8812AU/MT7921U TAPs). Linear regression fit: n=2.6.
	DefaultPathLossN = 2.6

	// NzymePathLossN matches DefaultPathLossN for consistency.
	// Legacy Nzyme data was n≈1.8 but that was for different hardware.
	NzymePathLossN = 2.6

	// DefaultShadowingSigmaDB is the typical log-normal shadowing standard deviation
	// Field data shows ~8 dB variance at similar distances
	DefaultShadowingSigmaDB = 8.0

	// Distance clamps - Inspire 3 detected at 36.4km
	MinDistanceM = 10.0
	MaxDistanceM = 50000.0

	// Valid RSSI range
	RSSIMin         = -120.0 // Below this is noise floor
	RSSIMax         = -20.0  // Above this is touching the antenna
	RSSIPlaceholder = -100.0 // Common placeholder value
)

// EnvironmentType represents the RF propagation environment
type EnvironmentType int

const (
	// EnvironmentOpenField is open field/rural with clear LOS from elevated sensors (n ~ 2.6)
	// Calibrated from Skylens field data (40 data points, 3-36km, RTL8814AU/RTL8812AU/MT7921U).
	// Must match DefaultPathLossN for consistency with BaseRSSI0 calibration.
	EnvironmentOpenField EnvironmentType = iota
	// EnvironmentSuburban is suburban with some obstructions (n ~ 2.4)
	EnvironmentSuburban
	// EnvironmentUrban is urban with buildings and multipath (n ~ 2.7)
	EnvironmentUrban
	// EnvironmentDenseUrban is dense urban canyon environment (n ~ 3.2)
	EnvironmentDenseUrban
	// EnvironmentIndoor is indoor environment with walls (n ~ 3.5)
	EnvironmentIndoor
)

// String returns the environment type name
func (e EnvironmentType) String() string {
	switch e {
	case EnvironmentOpenField:
		return "open_field"
	case EnvironmentSuburban:
		return "suburban"
	case EnvironmentUrban:
		return "urban"
	case EnvironmentDenseUrban:
		return "dense_urban"
	case EnvironmentIndoor:
		return "indoor"
	default:
		return "unknown"
	}
}

// PathLossExponent returns the path loss exponent n for this environment
func (e EnvironmentType) PathLossExponent() float64 {
	switch e {
	case EnvironmentOpenField:
		return DefaultPathLossN // 2.6 — must match BaseRSSI0 calibration
	case EnvironmentSuburban:
		return 2.4
	case EnvironmentUrban:
		return 2.7
	case EnvironmentDenseUrban:
		return 3.2
	case EnvironmentIndoor:
		return 3.5
	default:
		return DefaultPathLossN
	}
}

// ShadowingSigma returns typical shadowing standard deviation for this environment
func (e EnvironmentType) ShadowingSigma() float64 {
	switch e {
	case EnvironmentOpenField:
		return 6.0 // Field data shows 6-8 dB at typical ranges
	case EnvironmentSuburban:
		return 7.0
	case EnvironmentUrban:
		return 6.0
	case EnvironmentDenseUrban:
		return 8.0
	case EnvironmentIndoor:
		return 10.0 // More variation indoors
	default:
		return DefaultShadowingSigmaDB
	}
}

// ParseEnvironmentType parses an environment type from string
func ParseEnvironmentType(s string) EnvironmentType {
	switch strings.ToLower(s) {
	case "open_field", "openfield", "open", "field":
		return EnvironmentOpenField
	case "suburban", "suburb":
		return EnvironmentSuburban
	case "urban", "city":
		return EnvironmentUrban
	case "dense_urban", "denseurban", "dense":
		return EnvironmentDenseUrban
	case "indoor", "inside":
		return EnvironmentIndoor
	default:
		return EnvironmentOpenField
	}
}

// EnvironmentConfig holds environment settings for RF propagation
type EnvironmentConfig struct {
	mu              sync.RWMutex
	globalEnv       EnvironmentType
	tapEnvironments map[string]EnvironmentType // per-tap overrides
	tapRSSIOffsets  map[string]float64         // per-tap RSSI calibration offsets (dB)
}

// Global environment configuration
var envConfig = &EnvironmentConfig{
	globalEnv:       EnvironmentOpenField,
	tapEnvironments: make(map[string]EnvironmentType),
	tapRSSIOffsets:  make(map[string]float64),
}

// SetGlobalEnvironment sets the default environment for all taps
func SetGlobalEnvironment(env EnvironmentType) {
	envConfig.mu.Lock()
	defer envConfig.mu.Unlock()
	envConfig.globalEnv = env
}

// GetGlobalEnvironment returns the current global environment
func GetGlobalEnvironment() EnvironmentType {
	envConfig.mu.RLock()
	defer envConfig.mu.RUnlock()
	return envConfig.globalEnv
}

// SetTapEnvironment sets the environment for a specific tap
func SetTapEnvironment(tapID string, env EnvironmentType) {
	envConfig.mu.Lock()
	defer envConfig.mu.Unlock()
	envConfig.tapEnvironments[tapID] = env
}

// GetTapEnvironment returns the environment for a specific tap (or global default)
func GetTapEnvironment(tapID string) EnvironmentType {
	envConfig.mu.RLock()
	defer envConfig.mu.RUnlock()
	if env, ok := envConfig.tapEnvironments[tapID]; ok {
		return env
	}
	return envConfig.globalEnv
}

// ClearTapEnvironment removes per-tap environment override
func ClearTapEnvironment(tapID string) {
	envConfig.mu.Lock()
	defer envConfig.mu.Unlock()
	delete(envConfig.tapEnvironments, tapID)
}

// GetPathLossForTap returns the path loss exponent for a specific tap
func GetPathLossForTap(tapID string) float64 {
	return GetTapEnvironment(tapID).PathLossExponent()
}

// GetShadowingForTap returns the shadowing sigma for a specific tap
func GetShadowingForTap(tapID string) float64 {
	return GetTapEnvironment(tapID).ShadowingSigma()
}

// SetTapRSSIOffset sets a per-TAP RSSI calibration offset.
// Positive offset = TAP reads weaker than reference, add dB to normalize.
// Example: TAP-003 reads 16 dB weaker than co-located TAP-002 → offset = +16.
func SetTapRSSIOffset(tapID string, offsetDB float64) {
	envConfig.mu.Lock()
	defer envConfig.mu.Unlock()
	envConfig.tapRSSIOffsets[tapID] = offsetDB
}

// GetTapRSSIOffset returns the RSSI calibration offset for a TAP (0 if none set).
func GetTapRSSIOffset(tapID string) float64 {
	envConfig.mu.RLock()
	defer envConfig.mu.RUnlock()
	return envConfig.tapRSSIOffsets[tapID] // zero value if missing
}

// CalibrateRSSI applies per-TAP RSSI offset correction.
// Returns the corrected RSSI value for distance estimation.
func CalibrateRSSI(rssi float64, tapID string) float64 {
	offset := GetTapRSSIOffset(tapID)
	return rssi + offset
}

// ModelTXOffsets contains per-model TX power offsets relative to BaseRSSI0
// Validated against real flight data with GPS ground truth
var ModelTXOffsets = map[string]float64{
	// === RECALIBRATED FROM SKYLENS FIELD DATA (Feb 2026, n=2.6, BaseRSSI0=18.4) ===
	//
	// Field-calibrated models (GPS RemoteID ground truth):
	//   Mavic 3 Cine: -69 to -78 dBm @ 3-4km (34 pts) — BASELINE (offset=0)
	//   Air 3S:       -79 dBm @ 6.3km (3 pts)          — offset=+1.5
	//   Air 2S:       -95 dBm @ 12.1km (1 pt)           — offset=-7.2
	//   Mavic 2 Pro:  -89 dBm @ 8.7km (1 pt)            — offset=-4.9
	//   Inspire 3:    -94 dBm @ 36.4km (1 pt)            — offset=+6.2
	//
	// Uncalibrated models: scaled proportionally (×0.65) from old relative positions.
	// The offset spread narrows because n=2.6 absorbs more distance variation than n=1.8.

	// === Consumer drones (lower TX power) ===
	"Mini 2":         -10.0, // Scaled from -15.1 (Mini family: low TX power)
	"Mini SE":        -10.0,
	"Mini 3":         -8.0,
	"Mini 3 Pro":     -8.0,
	"Mini 4 Pro":     -8.0,
	"Mini (Unknown)": -8.0, // Family default for Mini

	// === FPV/Avata family (moderate TX power) ===
	"Avata":          -5.0,
	"Avata 2":        -5.0,
	"FPV":            -3.0,
	"FPV (Unknown)":  -4.0, // Family default for FPV/Avata
	"Neo":            -3.0,
	"Neo 2":          -3.0,  // 151g, similar TX power to Neo
	"Flip":           -3.0,

	// === Air family — FIELD CALIBRATED ===
	"Air 2":          0.0,
	"Air 2S":         -7.2,  // FIELD CALIBRATED @n=2.6: -95 dBm @ 12.1km
	"Air 3":          1.0,   // Scaled from 2.0
	"Air 3S":         1.5,   // FIELD CALIBRATED @n=2.6: -79 dBm @ 6.3km (3 pts)
	"Air (Unknown)":  1.0,   // Family default for Air

	// === Mavic family — FIELD CALIBRATED ===
	"Mavic 2 Pro":        -4.9,  // FIELD CALIBRATED @n=2.6: -89 dBm @ 8.7km
	"Mavic 3":            0.0,   // FIELD CALIBRATED: same platform as Mavic 3 Cine
	"Mavic 3 Classic":    0.0,
	"Mavic 3 Pro":        0.0,
	"Mavic 3 Cine":       0.0,   // FIELD CALIBRATED @n=2.6: baseline (34 pts, 3-4km)
	"Mavic 3 Enterprise": -5.0,  // Scaled from -7.4
	"Mavic (Unknown)":    0.0,   // Family default for Mavic
	"Mavic 4":            0.0,
	"Mavic 4 Pro":        0.0,

	// === Phantom family (older prosumer) ===
	"Phantom 4":          2.0,
	"Phantom 4 Pro":      3.0,
	"Phantom 4 Pro V2":   3.0,
	"Phantom (Unknown)":  2.0, // Family default for Phantom

	// === Enterprise/Commercial (highest TX power) ===
	"Matrice 4E":      -5.0,  // Live calibration: enterprise drones use lower 2.4GHz TX for RemoteID beacons
	"Matrice 4T":      -6.1,  // FIELD CALIBRATED: -86 dBm @ 6,058m → RSSI_0=12.3, offset from base=−6.1
	"Matrice 4":       5.0,
	"Matrice 30":      7.0,  // Scaled from 10.0
	"Matrice 30T":     7.0,
	"Matrice 350 RTK": 7.0,
	"Matrice 300 RTK": 7.0,

	// === Inspire family — FIELD CALIBRATED ===
	"Inspire 2":         5.0,  // Scaled from 8.0
	"Inspire 3":         6.2,  // FIELD CALIBRATED @n=2.6: -94 dBm @ 36.4km
	"Inspire (Unknown)": 5.0,  // Family default for Inspire

	// === Agras family (agricultural, high power for range) ===
	"Agras T10":         7.0,  // Scaled from 10.0
	"Agras T20":         7.0,
	"Agras T30":         7.0,  // Scaled from 11.0
	"Agras T40":         7.0,
	"Agras (Unknown)":   7.0,  // Family default for Agras

	// === DJI Controllers (WiFi hotspot, 5 GHz) ===
	// Controllers broadcast PROJ* SSID as 5GHz WiFi AP (~20-23 dBm EIRP).
	// No GPS for live calibration — keep conservative baseline.
	"DJI RC Controller":  0.0,
	"DJI RC Enterprise":  0.0, // Enterprise controllers (RM E70536 etc.) — similar TX power

	// === Generic DJI fallback ===
	"DJI (Unknown)": 0.0, // Conservative baseline

	// === 2025-2026 DJI releases (estimated from platform) ===
	"Mini 5":                 -8.0,
	"Mini 5 Pro":             -8.0,
	"Air 4":                  1.0,
	"Air 4 Pro":              1.0,
	"Avata 3":                -5.0,
	"FPV 2":                  -3.0,
	"Matrice 4S":             5.0,
	"Matrice 30 Pro":         7.0,
	"Mavic 4 Multispectral":  0.0,

	// === Other manufacturers ===
	"Autel EVO":      0.0,
	"Autel EVO II":   3.0,
	"Autel EVO III":  3.0,
	"Autel Dragonfish": 5.0,
	"Parrot Anafi":   -3.0,
	"Parrot Anafi AI": -2.0,
	"Skydio 2":       0.0,
	"Skydio 2+":      0.0,
	"Skydio X2":      2.0,
	"Skydio X10":     3.0,
	"GoPro Karma":    -7.0,
	"Yuneec Typhoon": -3.0,
	"Holy Stone":     -10.0,
}

// SkylensCalibrationPoints contains field-observed RSSI vs distance data
// from our TAPs (RTL8814AU, RTL8812AU, MT7921U) with GPS RemoteID ground truth.
// These are the points used for the current n=2.6 / BaseRSSI0=18.4 calibration.
var SkylensCalibrationPoints = []struct {
	DistanceM float64
	RSSI      float64
	Model     string
}{
	{3000, -69, "Mavic 3 Cine"},  // Closest pass
	{3200, -71, "Mavic 3 Cine"},
	{3500, -74, "Mavic 3 Cine"},
	{4000, -78, "Mavic 3 Cine"},  // 34 points in 3-4km range
	{6300, -79, "Air 3S"},        // 3 consistent readings
	{8700, -89, "Mavic 2 Pro"},   // OcuSync 2.0
	{12100, -95, "Air 2S"},       // Near noise floor
	{36400, -94, "Inspire 3"},    // Max observed range
}

// NzymeCalibrationPoints contains legacy field-observed RSSI vs distance data
// from Nzyme hardware (different TAP/antenna characteristics).
// Retained for reference — NOT used for current calibration.
var NzymeCalibrationPoints = []struct {
	DistanceM float64
	RSSI      float64
	Model     string
}{
	{785, -94, "Unknown"},
	{795, -79, "Unknown"},      // Baseline reference
	{1527, -75, "Air 2S"},      // Strong signal
	{1839, -93, "Unknown"},
	{2302, -80, "Unknown"},
	{3817, -80, "Unknown"},     // Excellent propagation
	{3818, -86, "Unknown"},
	{4753, -89, "Matrice 4T"},  // Enterprise
	{4881, -85, "Air 3S"},
	{6381, -95, "Unknown"},
	{6538, -85, "Unknown"},
	{8295, -88, "Unknown"},
	{8538, -94, "Unknown"},
	{10144, -94, "Unknown"},
	{10414, -93, "Unknown"},
	{12885, -88, "Air 2S"},     // Exceptional range
	{12962, -90, "Unknown"},
	{14690, -92, "Unknown"},
	{14815, -88, "Air 2S"},     // Max observed range
}

// DJI Serial Number -> Model Code Mapping
// DJI serial format: XXXXYYYZZZZZZZZZ where YYY/YYYY is the model code
var dji4CharCodes = map[string]string{
	"F7FV": "Matrice 4E",
	"F7FT": "Matrice 4T",
	"F7FM": "Matrice 350 RTK",
	"F5GU": "Mini 3 Pro",
	"F5KU": "Mini 4 Pro",
	"F20U": "Mini 5 Pro",
	"F21U": "Mini 5",
	"F6RU": "Air 4",
	"F6SU": "Air 4 Pro",
	"F5PU": "Mavic 4 Pro",
	"F5QU": "Mavic 4",
	"F5RU": "Mavic 4 Multispectral",
	"F4XU": "Avata 3",
	"F4YU": "FPV 2",
	"F5NU": "Inspire 3",
	"F7GU": "Matrice 4S",
	"F8CU": "Matrice 30 Pro",
}

var dji3CharCodes = map[string]string{
	"F4Q": "Mini 2",
	"F4R": "Mini SE",
	"F5G": "Mini 3 Pro",
	"F5K": "Mini 4 Pro",
	"F5M": "Mini 3",
	"F5A": "Air 2S",
	"F5B": "Mavic 3 Cine", // Field-confirmed 1581F5BK = Mavic 3 Cine (was incorrectly "Air 2")
	"F5C": "Air 3",
	"F5F": "Mavic 3",
	"F5H": "Mavic 3 Classic",
	"F5J": "Mavic 3 Pro",
	"F7F": "Matrice Series",
	"F89": "Mavic 3 Enterprise",
	"F8A": "Matrice 30",
	"F8B": "Matrice 30T",
	"F5D": "Avata",
	"F5E": "Avata 2",
	"F4S": "FPV",
	"F4T": "Avata",
	"F4U": "Avata 2",
	"F4V": "Neo",
	"F4W": "Flip",
	"F5P": "Mavic 4 Pro",
	"F5Q": "Mavic 4",
	"F6N": "Air 2S",
	"F6P": "Air 3",
	"F6Q": "Air 3S",
	"F20": "Mini 5 Pro",
	// 2025-2026 releases
	"F21": "Mini 5",
	"F5R": "Mavic 4 Multispectral",
	"F6R": "Air 4",
	"F6S": "Air 4 Pro",
	"F7G": "Matrice 4S",
	"F8C": "Matrice 30 Pro",
	"F5N": "Inspire 3",
	"F4X": "Avata 3",
	"F4Y": "FPV 2",
}

var djiValidPrefixes = []string{"158", "531", "1SS", "3NZ", "4NE"}

// GetModelFromSerial extracts DJI model name from RemoteID serial number
func GetModelFromSerial(serial string) string {
	if len(serial) < 4 {
		return ""
	}

	// First try the prefix-based lookup from dji_parser.go
	if model := DecodeModelFromSerial(serial); model != "" {
		return model
	}

	// Fall back to char-code based lookup
	if len(serial) < 8 {
		return ""
	}

	// Check for valid DJI prefix
	prefix := strings.ToUpper(serial[:3])
	valid := false
	for _, p := range djiValidPrefixes {
		if prefix == p {
			valid = true
			break
		}
	}
	if !valid {
		return ""
	}

	// Try 4-char code first (more specific)
	if len(serial) >= 8 {
		code4 := strings.ToUpper(serial[4:8])
		if model, ok := dji4CharCodes[code4]; ok {
			return model
		}
	}

	// Fall back to 3-char code
	code3 := strings.ToUpper(serial[4:7])
	if model, ok := dji3CharCodes[code3]; ok {
		return model
	}

	return ""
}

// GetRSSI0ForModel returns the reference RSSI at 1 meter for a given model
func GetRSSI0ForModel(model string) float64 {
	if model != "" {
		if offset, ok := ModelTXOffsets[model]; ok {
			return BaseRSSI0 + offset
		}
	}
	return GenericRSSI0
}

// EstimateDistanceFromRSSI estimates distance from sensor using RSSI with per-model calibration
// Returns distance in meters or -1 if invalid
func EstimateDistanceFromRSSI(rssi float64, model string, n float64) float64 {
	if rssi >= 0 {
		return -1
	}

	// Sanity check RSSI range
	if rssi < RSSIMin || rssi > RSSIMax {
		return -1
	}

	// Use default path loss if not specified
	if n == 0 {
		n = DefaultPathLossN
	}

	// Get model-specific or generic RSSI_0
	rssi0 := GetRSSI0ForModel(model)

	exponent := (rssi0 - rssi) / (10 * n)
	distance := math.Pow(10, exponent)

	// Clamp to reasonable range
	if distance < MinDistanceM {
		distance = MinDistanceM
	}
	if distance > MaxDistanceM {
		distance = MaxDistanceM
	}

	return math.Round(distance*10) / 10
}

// DistanceEstimate holds distance estimation result with confidence
type DistanceEstimate struct {
	Distance   float64 // Distance in meters
	Confidence float64 // Confidence 0.0-1.0
	ModelUsed  string  // Model used for calibration
}

// DistanceEstimateWithBounds holds distance estimation with uncertainty intervals
// Based on log-normal shadowing model:
//   d_min = d_hat * 10^(-sigma_dB / (10*n))
//   d_max = d_hat * 10^(+sigma_dB / (10*n))
type DistanceEstimateWithBounds struct {
	DistanceM    float64 `json:"distance_m"`     // Best estimate distance in meters
	DistanceMinM float64 `json:"distance_min_m"` // Lower bound (1-sigma)
	DistanceMaxM float64 `json:"distance_max_m"` // Upper bound (1-sigma)
	Confidence   float64 `json:"confidence"`     // Confidence score 0.0-1.0
	ModelUsed    string  `json:"model_used"`     // Model used for calibration
	Environment  string  `json:"environment"`    // Environment type used
	PathLossN    float64 `json:"path_loss_n"`    // Path loss exponent used
	SigmaDB      float64 `json:"sigma_db"`       // Shadowing sigma used

	// Signal quality metrics (for honest uncertainty reporting)
	SignalQuality    string  `json:"signal_quality"`     // "excellent", "good", "fair", "poor", "unreliable"
	MultipathWarning bool    `json:"multipath_warning"`  // True if RSSI variance suggests multipath
	RSSIVariance     float64 `json:"rssi_variance"`      // RSSI variance if available
}

// SignalQualityThresholds defines RSSI variance thresholds for quality assessment
const (
	ExcellentVarianceMax = 3.0  // <3dB variance = excellent (stable signal)
	GoodVarianceMax      = 6.0  // 3-6dB variance = good
	FairVarianceMax      = 10.0 // 6-10dB variance = fair (some multipath)
	PoorVarianceMax      = 15.0 // 10-15dB variance = poor (significant multipath)
	// >15dB = unreliable (severe multipath or movement artifacts)
)

// UncertaintyInflation multipliers based on signal quality
var uncertaintyInflation = map[string]float64{
	"excellent":  1.0,  // No inflation
	"good":       1.2,  // 20% wider bounds
	"fair":       1.5,  // 50% wider bounds
	"poor":       2.0,  // Double the uncertainty
	"unreliable": 3.0,  // Triple the uncertainty
}

// EstimateDistanceWithBounds estimates distance with uncertainty bounds
// Uses log-normal shadowing model to compute confidence intervals
// sigma_dB: log-normal shadowing standard deviation (typical: 4-10 dB)
// Returns distance with min/max bounds representing 1-sigma (68%) confidence interval
func EstimateDistanceWithBounds(rssi float64, model string, tapID string) DistanceEstimateWithBounds {
	env := GetTapEnvironment(tapID)
	n := env.PathLossExponent()
	sigmaDB := env.ShadowingSigma()

	// Apply per-TAP RSSI calibration offset
	rssi = CalibrateRSSI(rssi, tapID)

	return EstimateDistanceWithBoundsCustom(rssi, model, n, sigmaDB, env.String())
}

// EstimateDistanceWithBoundsCustom estimates distance with custom parameters
func EstimateDistanceWithBoundsCustom(rssi float64, model string, n float64, sigmaDB float64, envName string) DistanceEstimateWithBounds {
	result := DistanceEstimateWithBounds{
		DistanceM:    -1,
		DistanceMinM: -1,
		DistanceMaxM: -1,
		Confidence:   0,
		ModelUsed:    model,
		Environment:  envName,
		PathLossN:    n,
		SigmaDB:      sigmaDB,
	}

	// Validate RSSI
	if rssi >= 0 || rssi < RSSIMin || rssi > RSSIMax {
		return result
	}

	// Use default path loss if not specified
	if n == 0 {
		n = DefaultPathLossN
		result.PathLossN = n
	}

	// Use default sigma if not specified
	if sigmaDB == 0 {
		sigmaDB = DefaultShadowingSigmaDB
		result.SigmaDB = sigmaDB
	}

	// Get model-specific or generic RSSI_0
	rssi0 := GetRSSI0ForModel(model)

	// Calculate best estimate distance: d = 10^((RSSI_0 - RSSI) / (10*n))
	exponent := (rssi0 - rssi) / (10 * n)
	dHat := math.Pow(10, exponent)

	// Calculate uncertainty bounds using log-normal shadowing
	// At 1-sigma: RSSI varies by +/- sigma_dB
	// d_min = d_hat * 10^(-sigma_dB / (10*n))  -- RSSI higher means closer
	// d_max = d_hat * 10^(+sigma_dB / (10*n))  -- RSSI lower means farther
	uncertaintyFactor := math.Pow(10, sigmaDB/(10*n))
	dMin := dHat / uncertaintyFactor
	dMax := dHat * uncertaintyFactor

	// Clamp to reasonable range
	dHat = clampDistance(dHat)
	dMin = clampDistance(dMin)
	dMax = clampDistance(dMax)

	// Calculate confidence score
	confidence := calculateConfidence(rssi, model, sigmaDB)

	// Set model used
	usedModel := model
	if usedModel == "" {
		usedModel = "generic"
	}

	result.DistanceM = math.Round(dHat*10) / 10
	result.DistanceMinM = math.Round(dMin*10) / 10
	result.DistanceMaxM = math.Round(dMax*10) / 10
	result.Confidence = math.Round(confidence*100) / 100
	result.ModelUsed = usedModel

	return result
}

// clampDistance clamps distance to valid range
func clampDistance(d float64) float64 {
	if d < MinDistanceM {
		return MinDistanceM
	}
	if d > MaxDistanceM {
		return MaxDistanceM
	}
	return d
}

// calculateConfidence computes confidence score based on signal quality and calibration
func calculateConfidence(rssi float64, model string, sigmaDB float64) float64 {
	// Base confidence starts at 0.5
	confidence := 0.5

	// Boost confidence if model is known and calibrated
	if model != "" {
		if _, ok := ModelTXOffsets[model]; ok {
			confidence += 0.25 // Known model with calibration data
		}
	}

	// Adjust based on RSSI signal strength
	// Strong signals are more reliable
	if rssi > -50 {
		confidence += 0.15 // Very strong signal
	} else if rssi > -60 {
		confidence += 0.10 // Strong signal
	} else if rssi > -70 {
		confidence += 0.05 // Good signal
	} else if rssi < -90 {
		confidence -= 0.15 // Weak signal, more noise
	} else if rssi < -85 {
		confidence -= 0.10 // Moderately weak
	}

	// Reduce confidence for placeholder values
	if math.Abs(rssi-RSSIPlaceholder) < 0.5 {
		confidence -= 0.25
	}

	// Adjust based on environment uncertainty (higher sigma = lower confidence)
	if sigmaDB > 8 {
		confidence -= 0.10 // High uncertainty environment
	} else if sigmaDB < 5 {
		confidence += 0.05 // Low uncertainty environment
	}

	// Clamp to [0, 1]
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}

	return confidence
}

// EstimateDistanceWithConfidence estimates distance with confidence score
func EstimateDistanceWithConfidence(rssi float64, model string) DistanceEstimate {
	distance := EstimateDistanceFromRSSI(rssi, model, DefaultPathLossN)

	if distance < 0 {
		return DistanceEstimate{Distance: -1, Confidence: 0, ModelUsed: ""}
	}

	confidence := calculateConfidence(rssi, model, DefaultShadowingSigmaDB)

	usedModel := model
	if usedModel == "" {
		usedModel = "generic"
	}

	return DistanceEstimate{
		Distance:   distance,
		Confidence: math.Round(confidence*100) / 100,
		ModelUsed:  usedModel,
	}
}

// EstimateDistanceWithVariance estimates distance with RSSI variance-aware uncertainty
// This is the HONEST function - it inflates uncertainty when signal quality is poor
// rssiVariance: variance of recent RSSI samples (from RSSITracker.MobilityProfile.RSSIVariance)
func EstimateDistanceWithVariance(rssi float64, model string, tapID string, rssiVariance float64) DistanceEstimateWithBounds {
	env := GetTapEnvironment(tapID)
	n := env.PathLossExponent()
	sigmaDB := env.ShadowingSigma()

	// Apply per-TAP RSSI calibration offset
	rssi = CalibrateRSSI(rssi, tapID)

	// Get base estimate
	result := EstimateDistanceWithBoundsCustom(rssi, model, n, sigmaDB, env.String())

	// Assess signal quality based on RSSI variance
	quality, multipathWarning := assessSignalQuality(rssiVariance)
	result.SignalQuality = quality
	result.MultipathWarning = multipathWarning
	result.RSSIVariance = rssiVariance

	// Inflate uncertainty based on signal quality.
	// Uses multiplicative scaling (correct for log-domain distance estimation):
	//   d_min = d_hat / inflation_factor
	//   d_max = d_hat * inflation_factor
	// Previously used linear (additive) math which produced asymmetric/incorrect bounds.
	if inflation, ok := uncertaintyInflation[quality]; ok && inflation > 1.0 {
		result.DistanceMinM = math.Max(MinDistanceM, result.DistanceMinM/inflation)
		result.DistanceMaxM = math.Min(MaxDistanceM, result.DistanceMaxM*inflation)

		// Round
		result.DistanceMinM = math.Round(result.DistanceMinM*10) / 10
		result.DistanceMaxM = math.Round(result.DistanceMaxM*10) / 10

		// Reduce confidence proportionally
		result.Confidence = result.Confidence / inflation
		if result.Confidence < 0.1 {
			result.Confidence = 0.1
		}
		result.Confidence = math.Round(result.Confidence*100) / 100
	}

	return result
}

// assessSignalQuality determines signal quality and multipath warning from RSSI variance
func assessSignalQuality(rssiVariance float64) (string, bool) {
	if rssiVariance <= 0 {
		return "unknown", false
	}

	switch {
	case rssiVariance <= ExcellentVarianceMax:
		return "excellent", false
	case rssiVariance <= GoodVarianceMax:
		return "good", false
	case rssiVariance <= FairVarianceMax:
		return "fair", true // Multipath likely
	case rssiVariance <= PoorVarianceMax:
		return "poor", true
	default:
		return "unreliable", true
	}
}

// CalculateSlantDistance calculates slant (3D) distance from ground distance and altitude
// RF propagation follows slant distance, not ground distance
func CalculateSlantDistance(groundDistance, altitude float64) float64 {
	if altitude <= 0 {
		return groundDistance
	}
	return math.Sqrt(groundDistance*groundDistance + altitude*altitude)
}

// ReverseCalibrate calculates RSSI_0 from known RSSI and distance (for calibration)
func ReverseCalibrate(rssi, knownDistance, n float64) float64 {
	if knownDistance <= 0 {
		return GenericRSSI0
	}
	if n == 0 {
		n = DefaultPathLossN
	}
	return rssi + 10*n*math.Log10(knownDistance)
}

// GetAllKnownModels returns list of all models with calibration data
func GetAllKnownModels() []string {
	models := make([]string, 0, len(ModelTXOffsets))
	for model := range ModelTXOffsets {
		models = append(models, model)
	}
	return models
}
