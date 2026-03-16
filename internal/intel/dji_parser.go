package intel

import (
	"encoding/binary"
	"fmt"
	"math"
)

// DJI DroneID OUI for vendor IE detection (verified via field observations)
var DJIDroneIDOUIs = [][]byte{
	{0x26, 0x37, 0x12}, // Primary DJI DroneID OUI (vendor IE, not MAC)
	{0x26, 0x6F, 0x48}, // Alternate DJI OUI (vendor IE)
	{0x60, 0x60, 0x1F}, // DJI OcuSync v1/v2 (IEEE verified: SZ DJI TECHNOLOGY)
	// NOTE: Only add new OUIs after field verification with actual drone captures
}

// DJI coordinate conversion factor
// raw int32 / 174533.0 = degrees directly (174533 = 1e7 * PI / 180)
const djiCoordFactor = 174533.0

// DJIDroneID contains parsed DJI-specific DroneID fields
type DJIDroneID struct {
	// Product identification
	ProductType  uint8  // DJI product type code
	ProductModel string // Decoded model name

	// Drone identification
	SerialNumber string // Flight controller serial (16 chars)
	DroneUUID    []byte // 20-byte drone UUID

	// Current position
	Latitude  float64
	Longitude float64
	Altitude  float32 // Pressure altitude in meters
	Height    float32 // Height AGL in meters

	// Home/takeoff position
	HomeLat float64
	HomeLon float64

	// Velocity
	VelocityX float32 // m/s North
	VelocityY float32 // m/s East
	VelocityZ float32 // m/s Up (positive = climbing)
	Speed     float32 // Ground speed m/s

	// Orientation
	Yaw   float32 // Heading degrees (0-360)
	Pitch float32 // Pitch degrees
	Roll  float32 // Roll degrees

	// Pilot/operator
	PilotLat float64
	PilotLon float64

	// State info
	StateInfo     uint16 // Bit flags
	MotorsOn      bool
	InAir         bool
	GPSValid      bool
	AltitudeValid bool

	// Metadata
	SequenceNumber uint16
	Version        uint8
}

// DJI Product Type codes (from reverse engineering and public documentation)
var djiProductTypes = map[uint8]string{
	// Phantom series
	0x10: "Phantom 3 Standard",
	0x11: "Phantom 3 Advanced",
	0x12: "Phantom 3 Professional",
	0x13: "Phantom 3 4K",
	0x14: "Phantom 3 SE",
	0x20: "Phantom 4",
	0x21: "Phantom 4 Pro",
	0x22: "Phantom 4 Pro V2",
	0x23: "Phantom 4 Advanced",
	0x24: "Phantom 4 RTK",

	// Mavic series
	0x30: "Mavic Pro",
	0x31: "Mavic Pro Platinum",
	0x32: "Mavic 2 Pro",
	0x33: "Mavic 2 Zoom",
	0x34: "Mavic 2 Enterprise",
	0x35: "Mavic Air",
	0x36: "Mavic Air 2",
	0x37: "Mavic Air 2S",
	0x38: "Mavic 3",
	0x39: "Mavic 3 Pro",
	0x3A: "Mavic 3 Cine",
	0x3B: "Mavic 3 Classic",
	0x3C: "Mavic 3 Enterprise",

	// Mini series
	0x40: "Mavic Mini",
	0x41: "Mini 2",
	0x42: "Mini SE",
	0x43: "Mini 3",
	0x44: "Mini 3 Pro",
	0x45: "Mini 4 Pro",
	0x46: "Mini 4K",
	// Mini 2025-2026 models
	0x47: "Mini 5",
	0x48: "Mini 5 Pro",
	0x49: "Mini 5 SE",

	// Mavic 2025-2026 models (OcuSync v3/v4)
	0x3D: "Mavic 4",
	0x3E: "Mavic 4 Pro",
	0x3F: "Mavic 4 Classic",

	// Air 2025-2026 models
	0x4A: "Air 4",
	0x4B: "Air 4S",
	0x4C: "Air 4 Pro",

	// Neo/Flip 2025-2026
	0x4D: "Neo 2",
	0x4E: "Neo 2 Pro",
	0x4F: "Flip 2",

	// Spark
	0x50: "Spark",

	// Inspire series
	0x60: "Inspire 1",
	0x61: "Inspire 2",
	0x62: "Inspire 3",

	// Matrice series
	0x70: "Matrice 100",
	0x71: "Matrice 200",
	0x72: "Matrice 210",
	0x73: "Matrice 210 RTK",
	0x74: "Matrice 300 RTK",
	0x75: "Matrice 30",
	0x76: "Matrice 30T",
	0x77: "Matrice 350 RTK",
	// Matrice 4 series (2025-2026)
	0x78: "Matrice 4E",
	0x79: "Matrice 4T",
	0x7A: "Matrice 4S",
	0x7B: "Matrice 4 RTK",

	// FPV series
	0x80: "FPV",
	0x81: "Avata",
	0x82: "Avata 2",
	// FPV 2025-2026 models
	0x83: "Avata 3",
	0x84: "FPV 2",
	0x85: "FPV Pro",

	// Dock series (2025-2026)
	0x90: "Dock 2",
	0x91: "Dock 3",

	// Delivery/Cargo (2025-2026)
	0xA0: "FlyCart 30",
	0xA1: "FlyCart 100",

	// Agricultural
	0xF0: "Agras MG-1",
	0xF1: "Agras T10",
	0xF2: "Agras T20",
	0xF3: "Agras T30",
	0xF4: "Agras T40",
}

// DJI serial number prefixes to model mapping
// Based on field observations, Nzyme intel (Feb 2026), and public documentation
// Serial format: [ProductCode][UniqueID] - use longest matching prefix
// IMPORTANT: Longer prefixes must be checked first (8 chars > 6 chars > 5 chars)
var djiSerialPrefixes = map[string]string{
	// === CTA-2063-A Mappings (verified Feb 2026 against FAA DOC, community data) ===
	// TAP extracts serial[4..7] (includes 'F' length code), Node matches longer prefixes.
	// Longest match wins (8-char checked before 7-char before 5-char fallbacks).

	// --- Verified field-observed mappings ---
	"1581F45T": "Mavic 3",          // FAA DOC serial range confirmed
	"1581F45":  "Mavic 3",          // Fallback for F45 variants
	"1581F67P": "Mavic 3 Classic",  // Community confirmed
	"1581F67Q": "Mavic 3 Classic",  // Community confirmed (same family as 67P)
	"1581F67":  "Mavic 3 Classic",  // Fallback for F67 variants
	"1581F3YT": "Air 2S",           // DJI FAQ: Air 2S serial starts 3YT
	"1581F3Y":  "Air 2S",           // Fallback for F3Y variants
	"1581F163": "Mavic 2 Pro",      // Community: 163 = Mavic 2 Pro
	"1581F16":  "Mavic 2",          // Fallback for F16 variants
	"1581F1WN": "Mavic Air 2",      // Community confirmed
	"1581F1W":  "Mavic Air 2",      // Fallback for F1W variants
	"1581F4QW": "Avata",            // FAA DOC confirmed
	"1581F4Q":  "Avata",            // Fallback for F4Q variants
	"1581F0M6": "Mavic 2 Zoom",     // Community confirmed
	"1581F0M":  "Mavic 2 Zoom",     // Fallback for F0M variants
	"1581F11V": "Phantom 4 Pro V2", // Community confirmed
	"1581F11":  "Phantom 4 Pro V2", // Fallback for F11 variants
	"1581F1SC": "Mavic Mini",       // Community confirmed
	"1581F1S":  "Mavic Mini",       // Fallback for F1S variants
	"1581F3NZ": "Mini 2",           // Community confirmed
	"1581F3N":  "Mini 2",           // Fallback for F3N variants
	"1581F6MK": "Mavic 3 Pro",      // Community confirmed
	"1581F6M":  "Mavic 3 Pro",      // Fallback for F6M variants

	// --- Field-observed but unverified model name ---
	"1581F5BK": "Mavic 3 Cine",     // Nzyme data
	"1581F5FK": "Mavic 3 Cine",     // Nzyme data
	"1581F5FH": "Mavic 3 Enterprise", // Propeller Aero docs: 5FH = M3E
	"1581F5B":  "Mavic 3",          // 5B variants
	"1581F5F":  "Mavic 3",          // 5F variants
	"1581F5":   "Mavic 3",          // 1581F5 family fallback
	"1581F6N8": "Air 2S",           // Field confirmed: serial 1581F6N8725170H3A1CJ = Air 2S (Feb 2026)
	"1581F6QA": "Air 3S",           // Nzyme data
	"1581F6Z":  "Air 3S",           // Field observed (Puerto Rico Feb 2026)
	"1581F895": "Phantom 4 Pro",    // Field observed
	"1581F89":  "Phantom 4 Pro",    // Phantom family
	"1581F8LQ": "Matrice 30",       // Field observed
	"1581F8L":  "Matrice 30",       // Matrice 30 series
	"1581F9DE": "Inspire 3",        // Field observed
	"1581F9D":  "Inspire 3",        // Inspire family
	"1581F7FV": "Matrice 4T",       // Nzyme data
	"1581F37QB": "FPV",             // Community confirmed, field detected Feb 2026
	"1581F37Q": "FPV",              // Fallback for F37Q variants
	"1581F4XF": "Neo 2",            // Field detected Mar 2026, TAP-3 confirmed
	"1581F4X":  "Neo 2",            // Neo 2 family fallback
	"1581F4AE": "Mini SE",          // Community: 4AE = Mini SE
	"1581F4DT": "Mini SE",          // Community: 4DT = Mini SE (alternate)
	"1581F6C5": "Mini 2 SE",        // Community: 6C5 = Mini 2 SE
	"1581F6C":  "Mini 2 SE",        // Fallback for F6C variants

	// === Non-DJI manufacturer serial prefixes ===
	"1748": "Autel EVO",            // Autel CTA-2063 prefix, field detected Feb 2026
	"1588E": "Parrot Anafi",        // Parrot CTA-2063 prefix
	"1668B": "Skydio",              // Skydio CTA-2063 prefix

	// === Fallback mappings ===
	"1581F1":  "Mini (Unknown)",    // Mini family fallback
	"1581F3":  "DJI (Unknown)",     // Could be Air 2S (3Y) or Mini 2 (3N)
	"1581F6":  "Air (Unknown)",     // Air family fallback
	"1581F":   "DJI (Unknown)",     // Base DJI fallback

	// === Mavic 2 series ===
	"163CF": "Mavic 2 Pro",
	"163DF": "Mavic 2 Zoom",
	"163EF": "Mavic 2 Enterprise",

	// === Air series (older prefixes) ===
	"1WMD": "Air 2S",
	"1WMC": "Mavic Air 2",
	"1W6A": "Mavic Air",
	"2FWA": "Air 3",
	"2FWB": "Air 3S",

	// === Mini series ===
	"1WMJL": "Mini 2",
	"1WMJK": "Mavic Mini",
	"1WMJM": "Mini SE",
	"3WMA":  "Mini 3",
	"3WMB":  "Mini 3 Pro",
	"4WMA":  "Mini 4 Pro",
	"4WMB":  "Mini 4K",

	// === FPV/Avata ===
	"1FGAA": "DJI FPV",
	"5LKA":  "Avata",
	"5LKB":  "Avata 2",

	// === Matrice/Enterprise ===
	"0YLDF": "Matrice 300 RTK",
	"1ZNDF": "Matrice 30",
	"1ZNEF": "Matrice 30T",
	"5ZNA":  "Matrice 350 RTK",

	// === Phantom 4 series ===
	"1A3DF": "Phantom 4 Pro",
	"1A3CF": "Phantom 4",
	"1A3EF": "Phantom 4 Pro V2",

	// === Inspire ===
	"0HYBF": "Inspire 2",
	"5HAA":  "Inspire 3",
}

// DecodeModelFromSerial attempts to identify the DJI drone model from its serial number
// Returns the model name if identified, empty string otherwise
func DecodeModelFromSerial(serial string) string {
	if len(serial) < 4 {
		return ""
	}

	// Try increasingly shorter prefixes (longest match wins)
	// IMPORTANT: Check 8-char prefixes first to distinguish Air 2S/3S from Mavic 3
	for prefixLen := 8; prefixLen >= 4; prefixLen-- {
		if len(serial) >= prefixLen {
			prefix := serial[:prefixLen]
			if model, ok := djiSerialPrefixes[prefix]; ok {
				return model
			}
		}
	}

	return ""
}

// ClassifyDroneType returns a classification based on the model name
// Returns: "consumer", "prosumer", "enterprise", "fpv_racing", "agricultural", "unknown"
func ClassifyDroneType(model string) string {
	if model == "" {
		return "unknown"
	}

	// Check for specific keywords
	modelUpper := model

	// Enterprise/Professional
	if contains(modelUpper, "Matrice") || contains(modelUpper, "Enterprise") ||
		contains(modelUpper, "RTK") || contains(modelUpper, "Inspire") ||
		contains(modelUpper, "FlyCart") || contains(modelUpper, "Dock") {
		return "enterprise"
	}

	// Agricultural
	if contains(modelUpper, "Agras") || contains(modelUpper, "T10") ||
		contains(modelUpper, "T20") || contains(modelUpper, "T30") || contains(modelUpper, "T40") {
		return "agricultural"
	}

	// FPV/Racing
	if contains(modelUpper, "FPV") || contains(modelUpper, "Avata") {
		return "fpv_racing"
	}

	// Consumer (Mini series)
	if contains(modelUpper, "Mini") || contains(modelUpper, "Spark") ||
		contains(modelUpper, "Neo") || contains(modelUpper, "Flip") {
		return "consumer"
	}

	// Prosumer (Mavic, Air, Phantom)
	if contains(modelUpper, "Mavic") || contains(modelUpper, "Air") ||
		contains(modelUpper, "Phantom") {
		return "prosumer"
	}

	return "unknown"
}

// Helper for case-insensitive contains
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		len(s) > len(substr) && (s[:len(substr)] == substr ||
		containsSubstr(s, substr)))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ParseDJIDroneID decodes DJI proprietary DroneID vendor IE
// The data parameter should be the vendor IE payload AFTER the OUI+type bytes
func ParseDJIDroneID(data []byte, subcommand uint8) (*DJIDroneID, error) {
	if len(data) < 53 {
		return nil, fmt.Errorf("DJI DroneID payload too short: %d bytes (need 53+)", len(data))
	}

	result := &DJIDroneID{}

	// Version: offset 0
	result.Version = data[0]

	// Sequence number: offset 1, uint16 LE
	result.SequenceNumber = binary.LittleEndian.Uint16(data[1:3])

	// State info: offset 3, uint16 LE
	result.StateInfo = binary.LittleEndian.Uint16(data[3:5])
	result.MotorsOn = (result.StateInfo & 0x01) != 0
	result.InAir = ((result.StateInfo >> 1) & 0x03) > 0
	result.GPSValid = ((result.StateInfo >> 3) & 0x01) != 0
	result.AltitudeValid = ((result.StateInfo >> 4) & 0x01) != 0

	// Serial number: offset 5, 16 bytes, ASCII null-terminated
	serial := extractASCII(data[5:21])
	if serial != "" {
		result.SerialNumber = serial
	}

	// DJI DroneID V2 (OcuSync 3/O4, firmware mid-2023+) encrypts telemetry fields.
	// Serial number remains cleartext, but coordinates, velocity, altitude, and heading
	// are ciphertext. Only trust serial, product_type, state_info, and sequence for V2+.
	if result.Version >= 2 {
		// Product type: offset 53 (still cleartext in V2)
		if len(data) >= 54 {
			result.ProductType = data[53]
			if model, ok := djiProductTypes[result.ProductType]; ok {
				result.ProductModel = model
			} else {
				result.ProductModel = fmt.Sprintf("Unknown DJI (0x%02X)", result.ProductType)
			}
		}
		return result, nil
	}

	// Longitude: offset 21, int32 LE (DJI coordinate format)
	// DJI encodes as degrees * 174533.0 (which is 1e7 * PI / 180)
	// Dividing by 174533.0 gives degrees directly — no extra radians conversion
	rawLon := int32(binary.LittleEndian.Uint32(data[21:25]))
	if rawLon != 0 {
		result.Longitude = float64(rawLon) / djiCoordFactor
	}

	// Latitude: offset 25, int32 LE
	rawLat := int32(binary.LittleEndian.Uint32(data[25:29]))
	if rawLat != 0 {
		result.Latitude = float64(rawLat) / djiCoordFactor
	}

	// Altitude (pressure): offset 29, int16 LE, meters
	altRaw := int16(binary.LittleEndian.Uint16(data[29:31]))
	result.Altitude = float32(altRaw)

	// Height AGL: offset 31, int16 LE, meters
	heightRaw := int16(binary.LittleEndian.Uint16(data[31:33]))
	result.Height = float32(heightRaw)

	// Velocity North: offset 33, int16 LE, cm/s -> m/s
	vNorth := int16(binary.LittleEndian.Uint16(data[33:35]))
	result.VelocityX = float32(vNorth) / 100.0

	// Velocity East: offset 35, int16 LE, cm/s -> m/s
	vEast := int16(binary.LittleEndian.Uint16(data[35:37]))
	result.VelocityY = float32(vEast) / 100.0

	// Velocity Up: offset 37, int16 LE, cm/s -> m/s (positive = climbing)
	vUp := int16(binary.LittleEndian.Uint16(data[37:39]))
	result.VelocityZ = float32(vUp) / 100.0

	// Calculate ground speed
	result.Speed = float32(math.Sqrt(float64(result.VelocityX*result.VelocityX + result.VelocityY*result.VelocityY)))

	// Pitch: offset 39, int16 LE, scale by 1/100 or 1/5729.6 for radians
	pitchRaw := int16(binary.LittleEndian.Uint16(data[39:41]))
	result.Pitch = float32(pitchRaw) / 100.0

	// Roll: offset 41, int16 LE
	rollRaw := int16(binary.LittleEndian.Uint16(data[41:43]))
	result.Roll = float32(rollRaw) / 100.0

	// Yaw: offset 43, int16 LE, divide by 5729.6 for radians, convert to degrees
	yawRaw := int16(binary.LittleEndian.Uint16(data[43:45]))
	result.Yaw = float32(yawRaw) / 100.0
	// Normalize to 0-360
	for result.Yaw < 0 {
		result.Yaw += 360
	}
	for result.Yaw >= 360 {
		result.Yaw -= 360
	}

	// Home longitude: offset 45, int32 LE
	rawHomeLon := int32(binary.LittleEndian.Uint32(data[45:49]))
	if rawHomeLon != 0 {
		result.HomeLon = float64(rawHomeLon) / djiCoordFactor
	}

	// Home latitude: offset 49, int32 LE
	rawHomeLat := int32(binary.LittleEndian.Uint32(data[49:53]))
	if rawHomeLat != 0 {
		result.HomeLat = float64(rawHomeLat) / djiCoordFactor
	}

	// Product type: offset 53, if available
	if len(data) > 53 {
		result.ProductType = data[53]
		if model, ok := djiProductTypes[result.ProductType]; ok {
			result.ProductModel = model
		} else {
			result.ProductModel = fmt.Sprintf("Unknown DJI (0x%02X)", result.ProductType)
		}
	}

	// UUID: offset 54 (length byte) + 55-74 (20 bytes), if available
	if len(data) >= 75 {
		uuidLen := data[54]
		if uuidLen > 0 && uuidLen <= 20 {
			result.DroneUUID = make([]byte, uuidLen)
			copy(result.DroneUUID, data[55:55+uuidLen])
		}
	}

	// Pilot location: may be at higher offsets in extended payloads
	if len(data) >= 88 {
		rawPilotLon := int32(binary.LittleEndian.Uint32(data[75:79]))
		rawPilotLat := int32(binary.LittleEndian.Uint32(data[79:83]))
		if rawPilotLon != 0 {
			result.PilotLon = float64(rawPilotLon) / djiCoordFactor
		}
		if rawPilotLat != 0 {
			result.PilotLat = float64(rawPilotLat) / djiCoordFactor
		}
	}

	return result, nil
}

// HasValidPosition returns true if GPS coordinates are valid
func (d *DJIDroneID) HasValidPosition() bool {
	return d.GPSValid && d.Latitude != 0 && d.Longitude != 0 &&
		math.Abs(d.Latitude) <= 90 && math.Abs(d.Longitude) <= 180
}

// HasHomePosition returns true if home/takeoff location is available
func (d *DJIDroneID) HasHomePosition() bool {
	return d.HomeLat != 0 && d.HomeLon != 0 &&
		math.Abs(d.HomeLat) <= 90 && math.Abs(d.HomeLon) <= 180
}

// HasPilotPosition returns true if pilot location is available
func (d *DJIDroneID) HasPilotPosition() bool {
	return d.PilotLat != 0 && d.PilotLon != 0 &&
		math.Abs(d.PilotLat) <= 90 && math.Abs(d.PilotLon) <= 180
}

// GetVerticalSpeed returns vertical speed (positive = climbing)
func (d *DJIDroneID) GetVerticalSpeed() float32 {
	return d.VelocityZ // DJI v_up field: positive = climbing
}

// IsDJIOUI checks if the given OUI belongs to DJI DroneID
func IsDJIOUI(oui []byte) bool {
	if len(oui) < 3 {
		return false
	}
	for _, djiOUI := range DJIDroneIDOUIs {
		if oui[0] == djiOUI[0] && oui[1] == djiOUI[1] && oui[2] == djiOUI[2] {
			return true
		}
	}
	return false
}

// GetDJIModelName returns the model name for a DJI product type code
func GetDJIModelName(productType uint8) string {
	if model, ok := djiProductTypes[productType]; ok {
		return model
	}
	return ""
}
