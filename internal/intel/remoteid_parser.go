package intel

import (
	"encoding/binary"
	"fmt"
	"math"
)

// RemoteIDMessageType per ASTM F3411
type RemoteIDMessageType uint8

const (
	MsgTypeBasicID    RemoteIDMessageType = 0x00
	MsgTypeLocation   RemoteIDMessageType = 0x01
	MsgTypeAuth       RemoteIDMessageType = 0x02
	MsgTypeSelfID     RemoteIDMessageType = 0x03
	MsgTypeSystem     RemoteIDMessageType = 0x04
	MsgTypeOperatorID RemoteIDMessageType = 0x05
	MsgTypePack       RemoteIDMessageType = 0x0F
)

func (m RemoteIDMessageType) String() string {
	switch m {
	case MsgTypeBasicID:
		return "BasicID"
	case MsgTypeLocation:
		return "Location"
	case MsgTypeAuth:
		return "Auth"
	case MsgTypeSelfID:
		return "SelfID"
	case MsgTypeSystem:
		return "System"
	case MsgTypeOperatorID:
		return "OperatorID"
	case MsgTypePack:
		return "MessagePack"
	default:
		return fmt.Sprintf("Unknown(0x%02X)", uint8(m))
	}
}

// UAType per ASTM F3411-19
type UAType uint8

const (
	UATypeNone           UAType = 0
	UATypeAeroplane      UAType = 1
	UATypeHelicopter     UAType = 2  // Includes multirotors
	UATypeGyroplane      UAType = 3
	UATypeHybridLift     UAType = 4  // VTOL
	UATypeOrnithopter    UAType = 5
	UATypeGlider         UAType = 6
	UATypeKite           UAType = 7
	UATypeFreeBalloon    UAType = 8
	UATypeCaptiveBalloon UAType = 9
	UATypeAirship        UAType = 10
	UATypeParachute      UAType = 11
	UATypeRocket         UAType = 12
	UATypeTethered       UAType = 13
	UATypeGroundObstacle UAType = 14
	UATypeOther          UAType = 15
)

func (t UAType) String() string {
	names := []string{
		"None", "Aeroplane", "Helicopter/Multirotor", "Gyroplane",
		"Hybrid Lift (VTOL)", "Ornithopter", "Glider", "Kite",
		"Free Balloon", "Captive Balloon", "Airship", "Parachute",
		"Rocket", "Tethered", "Ground Obstacle", "Other",
	}
	if int(t) < len(names) {
		return names[t]
	}
	return fmt.Sprintf("Unknown(%d)", t)
}

// IDType per ASTM F3411
type IDType uint8

const (
	IDTypeNone         IDType = 0
	IDTypeSerialNumber IDType = 1 // ANSI/CTA-2063-A Serial
	IDTypeCAA          IDType = 2 // CAA Assigned Registration ID
	IDTypeUTMAssigned  IDType = 3 // UTM Assigned UUID
	IDTypeSpecificSession IDType = 4 // Specific Session ID
)

// ParsedRemoteID contains all extracted RemoteID fields
type ParsedRemoteID struct {
	// Basic ID
	SerialNumber string
	IDType       IDType
	UAType       UAType

	// Location
	Latitude       float64
	Longitude      float64
	AltitudeGeo    float32 // Geodetic altitude (WGS84)
	AltitudeBaro   float32 // Barometric/pressure altitude
	HeightAGL      float32 // Height above ground level
	HeightRef      uint8   // 0=takeoff, 1=ground
	Speed          float32 // Ground speed m/s
	VerticalSpeed  float32 // Vertical speed m/s
	TrackDirection float32 // Track/heading degrees
	Timestamp      uint16  // 1/10 seconds since hour start
	TimestampValid bool
	AccuracyH      uint8 // Horizontal accuracy enum
	AccuracyV      uint8 // Vertical accuracy enum
	AccuracySpeed  uint8 // Speed accuracy enum
	BaroValid      bool
	HeightValid    bool

	// Operator/System
	OperatorLat        float64
	OperatorLon        float64
	OperatorAlt        float32
	OperatorLocType    uint8  // 0=takeoff, 1=live GNSS, 2=fixed
	AreaCount          uint8  // Number of aircraft in area
	AreaRadius         uint16 // meters
	AreaCeiling        float32
	AreaFloor          float32
	ClassificationType uint8
	CategoryEU         uint8
	ClassEU            uint8

	// Self-ID
	DescriptionType uint8
	Description     string

	// Auth
	AuthType      uint8
	AuthPageCount uint8
	AuthLength    uint8
	AuthTimestamp uint32
	AuthData      []byte

	// Operator ID
	OperatorID     string
	OperatorIDType uint8

	// Metadata
	ProtocolVersion uint8
	MessageTypes    []RemoteIDMessageType
}

// ParseRemoteIDPayload decodes ASTM F3411 vendor IE payload
// Handles both raw ASTM F3411 and NAN (WiFi Aware) wrapped payloads
func ParseRemoteIDPayload(payload []byte) (*ParsedRemoteID, error) {
	if len(payload) < 1 {
		return nil, fmt.Errorf("empty payload")
	}

	result := &ParsedRemoteID{
		MessageTypes: make([]RemoteIDMessageType, 0, 6),
	}

	// Determine starting offset - some payloads have NAN framing prefix
	// Valid ASTM F3411 message types: 0-5 (BasicID, Location, Auth, SelfID, System, OperatorID)
	// and 0x0F (MessagePack). Protocol version is typically 0-2.
	offset := 0
	msgType := RemoteIDMessageType(payload[0] >> 4)
	protoVer := payload[0] & 0x0F

	isValidASTM := (msgType <= MsgTypeOperatorID || msgType == MsgTypePack) && protoVer <= 2

	// NAN framing detection: byte[0] can look like valid ASTM (e.g. 0x22 = Auth v2)
	// but actually be a NAN prefix. Prefer MessagePack at byte[1] when available,
	// since a single Auth/SelfID before a MessagePack makes no structural sense.
	if len(payload) > 1 {
		nextMsgType := RemoteIDMessageType(payload[1] >> 4)
		nextProtoVer := payload[1] & 0x0F
		nextIsValid := (nextMsgType <= MsgTypeOperatorID || nextMsgType == MsgTypePack) && nextProtoVer <= 2

		if nextIsValid && nextMsgType == MsgTypePack {
			// Byte[1] is a MessagePack header — byte[0] is NAN framing
			offset = 1
			msgType = nextMsgType
		} else if !isValidASTM && nextIsValid {
			// Byte[0] invalid, byte[1] valid — skip NAN prefix
			offset = 1
			msgType = nextMsgType
		}
	}

	if offset > 0 {
		payload = payload[offset:]
		if len(payload) < 1 {
			return nil, fmt.Errorf("payload too short after skipping NAN header")
		}
		msgType = RemoteIDMessageType(payload[0] >> 4)
	}

	// Check for message pack (multiple messages in one IE)
	if msgType == MsgTypePack {
		return parseMessagePack(payload, result)
	}

	// Single message - 25 bytes per ASTM F3411
	return parseSingleMessage(payload, result)
}

func parseSingleMessage(data []byte, result *ParsedRemoteID) (*ParsedRemoteID, error) {
	if len(data) < 25 {
		return result, fmt.Errorf("message too short: %d bytes (need 25)", len(data))
	}

	msgType := RemoteIDMessageType(data[0] >> 4)
	protoVer := data[0] & 0x0F
	result.ProtocolVersion = protoVer
	result.MessageTypes = append(result.MessageTypes, msgType)

	switch msgType {
	case MsgTypeBasicID:
		parseBasicID(data[1:], result)
	case MsgTypeLocation:
		parseLocation(data[1:], result)
	case MsgTypeSystem:
		parseSystem(data[1:], result)
	case MsgTypeOperatorID:
		parseOperatorID(data[1:], result)
	case MsgTypeSelfID:
		parseSelfID(data[1:], result)
	case MsgTypeAuth:
		parseAuth(data[1:], result)
	}

	return result, nil
}

func parseMessagePack(data []byte, result *ParsedRemoteID) (*ParsedRemoteID, error) {
	if len(data) < 3 {
		return result, fmt.Errorf("message pack too short")
	}

	// Pack header: msg type (4 bits), version (4 bits), size (1 byte), count (1 byte)
	result.ProtocolVersion = data[0] & 0x0F
	msgSize := int(data[1])
	msgCount := int(data[2])

	if msgSize == 0 {
		msgSize = 25 // Default ASTM F3411 message size
	}

	offset := 3
	for i := 0; i < msgCount && offset+msgSize <= len(data); i++ {
		parseSingleMessage(data[offset:offset+msgSize], result)
		offset += msgSize
	}

	return result, nil
}

func parseBasicID(data []byte, result *ParsedRemoteID) {
	if len(data) < 24 {
		return
	}

	result.IDType = IDType((data[0] >> 4) & 0x0F)
	result.UAType = UAType(data[0] & 0x0F)

	// Serial is 20 ASCII chars, null-terminated
	serial := extractASCII(data[1:21])
	if serial != "" {
		result.SerialNumber = serial
	}
}

func parseLocation(data []byte, result *ParsedRemoteID) {
	if len(data) < 24 {
		return
	}

	// ASTM F3411-22a Location Message layout (after header byte):
	// Byte 0: SpeedMult(bit0) | EWDir(bit1) | HeightType(bit2) | Rsv(bit3) | Status(bits4-7)
	// Byte 1: Direction (0-179, +180 if EW flag set)
	// Byte 2: Speed (full byte, resolution depends on SpeedMult)
	// Byte 3: Vertical Speed (int8, * 0.5 m/s)
	// Bytes 4-7: Latitude (int32, 1e-7 degrees)
	// Bytes 8-11: Longitude (int32, 1e-7 degrees)
	// Bytes 12-13: Pressure Altitude
	// Bytes 14-15: Geodetic Altitude
	// Bytes 16-17: Height AGL
	// Byte 18: Vertical/Horizontal Accuracy
	// Byte 19: Baro/Speed Accuracy + Timestamp Accuracy
	// Bytes 20-21: Timestamp

	// Byte 0 bitfield (per opendroneid-core-c ODID_Location_encoded)
	speedMult := data[0] & 0x01
	ewDirection := (data[0] >> 1) & 0x01
	result.HeightRef = (data[0] >> 2) & 0x01
	// bits 4-7 = operational status (not individual validity flags)

	// Direction (byte 1): full byte, 0-179 range, add 180 if EW flag set
	if data[1] <= 179 {
		direction := uint16(data[1])
		if ewDirection != 0 {
			direction += 180
		}
		result.TrackDirection = float32(direction)
	}

	// Speed (byte 2): full byte, resolution depends on SpeedMult from byte 0
	// mult=0: raw * 0.25 m/s (range 0-63.75)
	// mult=1: raw * 0.75 + 63.75 m/s (range 63.75-254.25)
	speedRaw := data[2]
	if speedRaw != 255 { // 255 = unknown sentinel
		if speedMult == 0 {
			result.Speed = float32(speedRaw) * 0.25
		} else {
			result.Speed = float32(speedRaw)*0.75 + 63.75
		}
	}

	// Vertical speed (byte 3): int8 * 0.5 m/s
	// Per opendroneid-core-c: SpeedVertical is int8_t, decoded = raw_signed * 0.5
	vsRaw := int8(data[3])
	if vsRaw != 63 {
		result.VerticalSpeed = float32(vsRaw) * 0.5
	}

	// Latitude (scaled int32, value * 1e-7 = degrees)
	latRaw := int32(binary.LittleEndian.Uint32(data[4:8]))
	if latRaw != 0 {
		result.Latitude = float64(latRaw) * 1e-7
	}

	// Longitude
	lonRaw := int32(binary.LittleEndian.Uint32(data[8:12]))
	if lonRaw != 0 {
		result.Longitude = float64(lonRaw) * 1e-7
	}

	// Pressure altitude (0.5m resolution, offset -1000m)
	altPressRaw := binary.LittleEndian.Uint16(data[12:14])
	if altPressRaw != 0 && altPressRaw != 0xFFFF {
		result.AltitudeBaro = float32(altPressRaw)*0.5 - 1000
	}

	// Geodetic altitude
	altGeoRaw := binary.LittleEndian.Uint16(data[14:16])
	if altGeoRaw != 0 && altGeoRaw != 0xFFFF {
		result.AltitudeGeo = float32(altGeoRaw)*0.5 - 1000
	}

	// Height AGL
	heightRaw := binary.LittleEndian.Uint16(data[16:18])
	if heightRaw != 0 && heightRaw != 0xFFFF {
		result.HeightAGL = float32(heightRaw)*0.5 - 1000
	}

	// Accuracy
	result.AccuracyV = (data[18] >> 4) & 0x0F
	result.AccuracyH = data[18] & 0x0F
	result.AccuracySpeed = (data[19] >> 4) & 0x0F

	// Timestamp (1/10 second since hour start)
	result.Timestamp = binary.LittleEndian.Uint16(data[20:22])
}

func parseSystem(data []byte, result *ParsedRemoteID) {
	if len(data) < 24 {
		return
	}

	// ASTM F3411 System Message layout (after header byte):
	// Byte 0: Operator Location Type (2 bits) + Classification Type (3 bits) + reserved (3 bits)
	// Bytes 1-4: Operator Latitude (int32, 1e-7 degrees)
	// Bytes 5-8: Operator Longitude (int32, 1e-7 degrees)
	// Bytes 9-10: Area Count
	// Byte 11: Area Radius (uint8, *10m)
	// Bytes 12-13: Area Ceiling
	// Bytes 14-15: Area Floor
	// Byte 16: CategoryEU(bits0-3) | ClassEU(bits4-7)
	// Bytes 17-18: Operator Geodetic Altitude
	// Bytes 19-22: Timestamp

	flags := data[0]
	result.OperatorLocType = flags & 0x03
	result.ClassificationType = (flags >> 2) & 0x07

	// Operator location - starts at byte 1
	latRaw := int32(binary.LittleEndian.Uint32(data[1:5]))
	lonRaw := int32(binary.LittleEndian.Uint32(data[5:9]))
	if latRaw != 0 {
		result.OperatorLat = float64(latRaw) * 1e-7
	}
	if lonRaw != 0 {
		result.OperatorLon = float64(lonRaw) * 1e-7
	}

	// Area count and radius
	result.AreaCount = uint8(binary.LittleEndian.Uint16(data[9:11]))
	result.AreaRadius = uint16(data[11]) * 10 // Single byte, *10m resolution

	// Area ceiling/floor
	ceilRaw := binary.LittleEndian.Uint16(data[12:14])
	floorRaw := binary.LittleEndian.Uint16(data[14:16])
	if ceilRaw != 0 && ceilRaw != 0xFFFF {
		result.AreaCeiling = float32(ceilRaw)*0.5 - 1000
	}
	if floorRaw != 0 && floorRaw != 0xFFFF {
		result.AreaFloor = float32(floorRaw)*0.5 - 1000
	}

	// Category and Class (for EU classification)
	if len(data) > 16 {
		result.CategoryEU = data[16] & 0x0F
		result.ClassEU = (data[16] >> 4) & 0x0F
	}

	// Operator geodetic altitude
	opAltRaw := binary.LittleEndian.Uint16(data[17:19])
	if opAltRaw != 0 && opAltRaw != 0xFFFF {
		result.OperatorAlt = float32(opAltRaw)*0.5 - 1000
	}
}

func parseOperatorID(data []byte, result *ParsedRemoteID) {
	if len(data) < 24 {
		return
	}
	result.OperatorIDType = data[0]

	// Operator ID is 20 ASCII chars
	opid := extractASCII(data[1:21])
	if opid != "" {
		result.OperatorID = opid
	}
}

func parseSelfID(data []byte, result *ParsedRemoteID) {
	if len(data) < 24 {
		return
	}
	result.DescriptionType = data[0]

	// Description is 23 ASCII chars
	desc := extractASCII(data[1:24])
	if desc != "" {
		result.Description = desc
	}
}

func parseAuth(data []byte, result *ParsedRemoteID) {
	if len(data) < 24 {
		return
	}

	authHeader := data[0]
	result.AuthType = (authHeader >> 4) & 0x0F
	pageNum := authHeader & 0x0F

	if pageNum == 0 {
		// First page contains metadata
		result.AuthPageCount = data[1]
		result.AuthLength = data[2]
		result.AuthTimestamp = binary.LittleEndian.Uint32(data[3:7])
		result.AuthData = make([]byte, 17)
		copy(result.AuthData, data[7:24])
	} else {
		// Continuation page
		if result.AuthData == nil {
			result.AuthData = make([]byte, 0)
		}
		result.AuthData = append(result.AuthData, data[1:24]...)
	}
}

// extractASCII extracts null-terminated ASCII string from bytes
func extractASCII(data []byte) string {
	result := make([]byte, 0, len(data))
	for _, b := range data {
		if b == 0 {
			break
		}
		if b >= 0x20 && b <= 0x7E { // Printable ASCII
			result = append(result, b)
		}
	}
	return string(result)
}

// HasValidPosition returns true if the parsed data contains valid GPS coordinates
func (p *ParsedRemoteID) HasValidPosition() bool {
	return p.Latitude != 0 && p.Longitude != 0 &&
		math.Abs(p.Latitude) <= 90 && math.Abs(p.Longitude) <= 180
}

// HasOperatorPosition returns true if operator location is available
func (p *ParsedRemoteID) HasOperatorPosition() bool {
	return p.OperatorLat != 0 && p.OperatorLon != 0 &&
		math.Abs(p.OperatorLat) <= 90 && math.Abs(p.OperatorLon) <= 180
}

// AccuracyHMeters returns horizontal accuracy in meters
func (p *ParsedRemoteID) AccuracyHMeters() float32 {
	// ASTM F3411-19 accuracy table
	table := []float32{
		0, 0, 0, 0, // 0-3: Unknown/>=18.52km
		18520, 7408, 3704, 1852, // 4-7: >= values in meters
		926, 555.6, 185.2, 92.6, // 8-11
		30, 10, 3, 1, // 12-15
	}
	if int(p.AccuracyH) < len(table) {
		return table[p.AccuracyH]
	}
	return 0
}
