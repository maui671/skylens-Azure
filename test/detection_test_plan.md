# Skylens Detection Pipeline - Comprehensive Test Plan

## Executive Summary

This test plan addresses the critical gap where competitor Nzyme is catching drones that Skylens misses. The root cause analysis reveals that Skylens may be missing detections in several scenarios:

1. **RemoteID WiFi NAN frames** - ASTM F3411 compliance signals
2. **DJI DroneID vendor IEs** - Proprietary OcuSync beacons
3. **DJI Legacy beacons** - Older OUI format
4. **OUI-only detections** - WiFi fingerprinting without RemoteID
5. **SSID pattern matching** - Behavioral detection

This plan provides concrete test payloads, expected outcomes, and validation scripts.

---

## Test Infrastructure Overview

### NATS Topics
- `skylens.detections.{tap_id}` - Detection messages from TAPs
- `skylens.heartbeats.{tap_id}` - TAP heartbeats

### API Endpoints
| Endpoint | Purpose |
|----------|---------|
| `GET /api/drones` | List all tracked drones |
| `GET /api/drones/{id}` | Single drone details |
| `GET /api/taps` | List connected TAPs |
| `GET /api/suspects` | List suspect candidates |
| `GET /api/threat` | Threat assessment |
| `GET /api/fleet` | Fleet overview |
| `POST /api/test/drone` | Inject test drone |
| `POST /api/test/tap` | Inject test TAP |
| `POST /api/test/clear` | Clear test state |

### Protobuf Schema Reference
```protobuf
message Detection {
    string tap_id = 1;
    int64 timestamp_ns = 2;
    string mac_address = 3;
    string identifier = 4;
    string serial_number = 5;
    double latitude = 10;
    double longitude = 11;
    float altitude_geodetic = 12;
    float speed = 20;
    float heading = 22;
    double operator_latitude = 30;
    double operator_longitude = 31;
    string operator_id = 34;
    int32 rssi = 40;
    int32 channel = 41;
    string ssid = 43;
    DetectionSource source = 50;
    string manufacturer = 54;
    float confidence = 56;
    bytes remoteid_payload = 61;
}
```

---

## Test 1: ASTM F3411 RemoteID WiFi NAN Detection

### 1.1 High-Level Test Concept

- Validates that ASTM F3411-19 standard RemoteID broadcasts are correctly parsed
- Ensures all message types (BasicID, Location, System, OperatorID) flow through pipeline
- Confirms full telemetry (lat, lon, alt, speed, heading, operator_id) appears in API
- Critical for FAA compliance and standard drone detection

### 1.2 Test Design & Steps

**Injection Method**: NATS publish with protobuf message containing RemoteID payload

**RemoteID Payload Structure** (ASTM F3411):
```
Byte 0: Message Type (4 bits) | Protocol Version (4 bits)
Bytes 1-24: Message-specific payload (24 bytes)
Total: 25 bytes per message
```

**Message Pack Payload** (multi-message):
```
Byte 0: 0xF0 (MessagePack type) | Version
Byte 1: Message size (25)
Byte 2: Message count (4)
Bytes 3+: Individual 25-byte messages
```

**Test Payload - Complete RemoteID MessagePack**:
```go
// Build ASTM F3411 RemoteID message pack
// Contains: BasicID + Location + System + OperatorID

// BasicID (Message Type 0x00)
basicID := []byte{
    0x00,                   // Type 0, Version 0
    0x10,                   // IDType=SerialNumber(1), UAType=Helicopter(0)
    // 20-byte Serial Number (null-padded)
    '1', '5', '8', '1', 'F', '5', 'F', 'K', 'D', '2',
    '2', '9', '4', '0', '0', '0', '0', '0', 0, 0,
    0, 0, 0, 0,             // Reserved
}

// Location (Message Type 0x10)
location := []byte{
    0x10,                   // Type 1, Version 0
    0x2C,                   // Status: HeightValid, BaroValid, TimestampValid
    0x5A,                   // Direction: 180 degrees (90 * 2)
    0x22,                   // Speed: 8.5 m/s (34 * 0.25)
    0x00,                   // Speed multiplier
    0x7F,                   // Vertical speed: 0 m/s (63 + 0)/0.5
    // Latitude: 37.7749 degrees = 377749000 (scaled by 1e-7)
    0x98, 0x37, 0x86, 0x16, // Little-endian int32
    // Longitude: -122.4194 degrees = -1224194000
    0x30, 0xB9, 0x08, 0xB7, // Little-endian int32
    // Pressure altitude: 120m + 1000 = 2240 in 0.5m units
    0xC0, 0x08,             // 2240 LE
    // Geodetic altitude: 125m + 1000 = 2250
    0xCA, 0x08,
    // Height AGL: 50m + 1000 = 2100
    0x34, 0x08,
    // Accuracy: H=10m (0x0C), V=3m (0x0D)
    0xCD,
    0x00,                   // Speed accuracy
    // Timestamp: 1800 (180 seconds / 10)
    0x08, 0x07,
    0x00,                   // Reserved
}

// System (Message Type 0x40)
system := []byte{
    0x40,                   // Type 4, Version 0
    0x01,                   // OperatorLocType=LiveGNSS
    0x00,                   // Classification (none)
    // Operator Latitude: 37.7740
    0x30, 0x2E, 0x86, 0x16,
    // Operator Longitude: -122.4180
    0x60, 0xC4, 0x08, 0xB7,
    0x00, 0x00,             // Area count
    0x00, 0x00,             // Area radius
    0x00, 0x00,             // Area ceiling
    0x00, 0x00,             // Area floor
    0x00, 0x00,             // Operator altitude
    0x00, 0x00, 0x00, 0x00, // Reserved
}

// OperatorID (Message Type 0x50)
operatorID := []byte{
    0x50,                   // Type 5, Version 0
    0x00,                   // OperatorIDType
    // 20-byte Operator ID
    'F', 'A', 'A', '-', 'P', 'I', 'L', 'O', 'T', '-',
    '1', '2', '3', '4', '5', '6', 0, 0, 0, 0,
    0, 0, 0,                // Padding
}

// Combine into MessagePack
messagePack := []byte{0xF0, 25, 4} // Header
messagePack = append(messagePack, basicID...)
messagePack = append(messagePack, location...)
messagePack = append(messagePack, system...)
messagePack = append(messagePack, operatorID...)
```

**NATS Publish Command**:
```bash
# Using Go test helper
go run test/inject_remoteid.go --tap-id=tap-001 \
  --serial="1581F5FKD229400000" \
  --lat=37.7749 --lon=-122.4194 \
  --alt=120.0 --speed=8.5 --heading=180.0 \
  --operator-id="FAA-PILOT-123456" \
  --operator-lat=37.7740 --operator-lon=-122.4180
```

### 1.3 Expected Results / Assertions

**API Response** (`GET /api/drones`):
```json
{
  "drones": [
    {
      "identifier": "REMOTEID-1581F5FKD229400000",
      "mac": "60:60:1F:AA:BB:CC",
      "serial_number": "1581F5FKD229400000",
      "manufacturer": "DJI",
      "model": "Mavic 3",
      "latitude": 37.7749,
      "longitude": -122.4194,
      "altitude_geodetic": 125.0,
      "speed": 8.5,
      "heading": 180.0,
      "operator_id": "FAA-PILOT-123456",
      "operator_latitude": 37.7740,
      "operator_longitude": -122.4180,
      "detection_source": "SOURCE_WIFI_NAN",
      "trust_score": 90,
      "classification": "COMPLIANT",
      "status": "active"
    }
  ]
}
```

**Validation Checklist**:
- [ ] Serial number extracted from BasicID: `1581F5FKD229400000`
- [ ] Model identified from serial prefix: `Mavic 3` (F5F code)
- [ ] Latitude/Longitude within 0.0001 degrees of input
- [ ] Altitude (geodetic) within 1m of input
- [ ] Speed within 0.5 m/s of input
- [ ] Heading within 5 degrees of input
- [ ] Operator ID present and matches input
- [ ] Operator position within 0.001 degrees
- [ ] Detection source is `SOURCE_WIFI_NAN`
- [ ] Trust score >= 85 (RemoteID with full data)
- [ ] Classification is `COMPLIANT` or `NOMINAL`

**Latency Requirement**: Detection to API update < 100ms

---

## Test 2: DJI OcuSync DroneID Detection

### 2.1 High-Level Test Concept

- Validates DJI proprietary DroneID vendor IE parsing
- Tests OUI detection: `60:60:1F` (primary DJI OUI)
- Verifies SubCmd `0x13` (DroneID data frame) handling
- Confirms serial, model, position extraction from DJI-specific format

### 2.2 Test Design & Steps

**DJI DroneID Vendor IE Structure**:
```
OUI: 60:60:1F (or 26:37:12 for legacy)
SubCmd: 0x13 (DroneID)
Payload: 53+ bytes

Offset  Size  Description
0       1     Version
1       2     Sequence Number (LE)
3       2     State Info (LE) - motors, in_air, GPS flags
5       16    Serial Number (ASCII, null-padded)
21      4     Longitude (int32 LE, DJI format)
25      4     Latitude (int32 LE, DJI format)
29      2     Altitude (int16 LE, meters)
31      2     Height AGL (int16 LE, meters)
33      2     Velocity North (int16 LE, cm/s)
35      2     Velocity East (int16 LE, cm/s)
37      2     Velocity Down (int16 LE, cm/s)
39      2     Pitch (int16 LE, scaled)
41      2     Roll (int16 LE, scaled)
43      2     Yaw (int16 LE, scaled)
45      4     Home Longitude
49      4     Home Latitude
53      1     Product Type
54+     20    UUID (optional)
```

**DJI Coordinate Conversion**:
```
raw_value / 174533.0 * (180.0 / PI) = degrees
```

**Test Payload - DJI OcuSync Beacon**:
```go
// Build DJI DroneID payload
payload := make([]byte, 88)

// OUI (3 bytes) - placed at start for OUI detection
copy(payload[0:3], []byte{0x60, 0x60, 0x1F})

// SubCmd
payload[3] = 0x13

// DroneID data starts at offset 4
droneID := payload[4:]

// Version
droneID[0] = 0x02

// Sequence
binary.LittleEndian.PutUint16(droneID[1:3], 1234)

// State: MotorsOn | InAir | GPSValid | AltValid
droneID[3] = 0x0F
droneID[4] = 0x00

// Serial Number (16 bytes)
copy(droneID[5:21], []byte("1581F44KD234500123"))

// Longitude: -122.4194 degrees
// Radians: -2.1377
// DJI format: radians * 174533.0 / (180/PI) = raw
// Actually: degrees * 174533.0 * PI/180 = raw
// For -122.4194: -261543762
lonRaw := int32(-261543762)
binary.LittleEndian.PutUint32(droneID[21:25], uint32(lonRaw))

// Latitude: 37.7749 degrees
// Raw: 80738124
latRaw := int32(80738124)
binary.LittleEndian.PutUint32(droneID[25:29], uint32(latRaw))

// Altitude: 150m
binary.LittleEndian.PutUint16(droneID[29:31], 150)

// Height AGL: 80m
binary.LittleEndian.PutUint16(droneID[31:33], 80)

// Velocity North: 500 cm/s (5 m/s)
binary.LittleEndian.PutUint16(droneID[33:35], 500)

// Velocity East: 300 cm/s (3 m/s)
binary.LittleEndian.PutUint16(droneID[35:37], 300)

// Velocity Down: -100 cm/s (-1 m/s = climbing)
binary.LittleEndian.PutUint16(droneID[37:39], uint16(int16(-100)))

// Pitch: 0
binary.LittleEndian.PutUint16(droneID[39:41], 0)

// Roll: 0
binary.LittleEndian.PutUint16(droneID[41:43], 0)

// Yaw: 270 degrees (*100)
binary.LittleEndian.PutUint16(droneID[43:45], 27000)

// Product Type: 0x44 = Mini 3 Pro
droneID[53] = 0x44
```

**Detection Message with DJI Payload**:
```go
det := &pb.Detection{
    TapId:           "tap-001",
    TimestampNs:     time.Now().UnixNano(),
    MacAddress:      "60:60:1F:AA:BB:CC",
    Identifier:      "DJI-60:60:1F:AA:BB:CC",
    Rssi:            -55,
    Channel:         149,
    Source:          pb.DetectionSource_SOURCE_DJI_OCUSYNC,
    Manufacturer:    "DJI",
    RemoteidPayload: payload,
}
```

### 2.3 Expected Results / Assertions

**API Response**:
```json
{
  "identifier": "DJI-60:60:1F:AA:BB:CC",
  "mac": "60:60:1F:AA:BB:CC",
  "serial_number": "1581F44KD234500123",
  "manufacturer": "DJI",
  "model": "Mini 3 Pro",
  "latitude": 37.7749,
  "longitude": -122.4194,
  "altitude_pressure": 150.0,
  "height_agl": 80.0,
  "speed": 5.83,
  "heading": 270.0,
  "vertical_speed": 1.0,
  "detection_source": "SOURCE_DJI_OCUSYNC",
  "trust_score": 85,
  "classification": "COMPLIANT"
}
```

**Validation Checklist**:
- [ ] OUI `60:60:1F` recognized as DJI
- [ ] SubCmd `0x13` triggers DroneID parsing
- [ ] Serial extracted: `1581F44KD234500123`
- [ ] Model derived from product type `0x44`: Mini 3 Pro
- [ ] Coordinates converted correctly from DJI format
- [ ] Yaw/heading normalized to 0-360: 270.0
- [ ] Ground speed calculated: sqrt(5^2 + 3^2) = 5.83 m/s
- [ ] Vertical speed sign inverted: +1.0 m/s (climbing)

---

## Test 3: DJI Legacy Beacon Detection

### 3.1 High-Level Test Concept

- Validates detection of older DJI drones using legacy OUI `26:37:12`
- Ensures backward compatibility with pre-OcuSync drones
- Tests that legacy format parsing produces same output quality

### 3.2 Test Design & Steps

**Legacy OUI**: `26:37:12` (also `26:6F:48`)

**Payload Structure**: Same as OcuSync DroneID but with legacy OUI prefix

```go
// Legacy DJI beacon
payload := make([]byte, 88)

// Legacy OUI
copy(payload[0:3], []byte{0x26, 0x37, 0x12})

// SubCmd
payload[3] = 0x13

// Rest of payload identical to OcuSync format
// ... (same as Test 2)
```

**Detection Message**:
```go
det := &pb.Detection{
    TapId:           "tap-001",
    MacAddress:      "26:37:12:AA:BB:CC",
    Identifier:      "DJI-26:37:12:AA:BB:CC",
    Source:          pb.DetectionSource_SOURCE_WIFI_BEACON,
    RemoteidPayload: payload,
}
```

### 3.3 Expected Results / Assertions

- [ ] Legacy OUI `26:37:12` recognized by `IsDJIOUI()`
- [ ] Same parsing quality as OcuSync format
- [ ] Serial, model, position all extracted correctly
- [ ] Detection source shows `SOURCE_WIFI_BEACON`
- [ ] Trust score >= 80

---

## Test 4: OUI-Only Detection (SUSPECT Path)

### 4.1 High-Level Test Concept

- Tests WiFi fingerprinting when NO RemoteID payload is present
- Validates suspect correlation workflow
- Ensures OUI-only detections are NOT immediately promoted
- Confirms SUSPECT classification until correlation confirms

### 4.2 Test Design & Steps

**Scenario**: DJI OUI beacon without DroneID vendor IE

```go
det := &pb.Detection{
    TapId:           "tap-001",
    MacAddress:      "60:60:1F:AA:BB:CC",
    Identifier:      "SUSPECT-60:60:1F:AA:BB:CC",
    Ssid:            "",  // No SSID
    Rssi:            -65,
    Channel:         149,
    BeaconIntervalTu: 100,
    Source:          pb.DetectionSource_SOURCE_WIFI_BEACON,
    Manufacturer:    "UNKNOWN",
    Designation:     "SUSPECT",
    Confidence:      0.35,
    // NO remoteid_payload
}
```

**Multi-TAP Correlation Test**:
1. Inject detection from `tap-001` at T=0
2. Inject same MAC from `tap-002` at T=2s
3. Verify promotion after 2 TAPs observe

```bash
# TAP 1 detection
nats pub skylens.detections.tap-001 "$PAYLOAD_1"

# Wait 2 seconds
sleep 2

# TAP 2 detection (same MAC, different TAP)
nats pub skylens.detections.tap-002 "$PAYLOAD_2"
```

### 4.3 Expected Results / Assertions

**Before Correlation** (`GET /api/suspects`):
```json
{
  "suspects": [
    {
      "identifier": "SUSPECT-60:60:1F:AA:BB:CC",
      "mac_address": "60:60:1F:AA:BB:CC",
      "observations": 1,
      "taps_seen": {"tap-001": "2024-01-15T10:00:00Z"},
      "best_confidence": 0.35,
      "mobility_score": 0.0
    }
  ]
}
```

**After Multi-TAP Correlation** (`GET /api/drones`):
```json
{
  "drones": [
    {
      "identifier": "SUSPECT-60:60:1F:AA:BB:CC",
      "mac": "60:60:1F:AA:BB:CC",
      "manufacturer": "DJI",
      "detection_source": "MULTI_TAP_CORRELATED",
      "trust_score": 70,
      "classification": "CORRELATED",
      "spoof_flags": ["NO_REMOTEID"]
    }
  ]
}
```

**Validation Checklist**:
- [ ] Single-TAP detection remains in `/api/suspects`
- [ ] NOT promoted to `/api/drones` with single TAP
- [ ] After 2 TAPs within 30s, promoted to `/api/drones`
- [ ] `spoof_flags` includes `NO_REMOTEID`
- [ ] Trust score between 50-70 (no RemoteID penalty)
- [ ] Classification is `CORRELATED` or `UNVERIFIED`

---

## Test 5: SSID Pattern Detection

### 5.1 High-Level Test Concept

- Tests SSID-based drone identification
- Validates pattern matching for DJI, Parrot, Autel, etc.
- Confirms model extraction from SSID (e.g., "DJI-MAVIC-3-XXXX")
- Tests case insensitivity

### 5.2 Test Design & Steps

**Test Cases**:
| SSID Pattern | Expected Manufacturer | Expected Model/Hint |
|--------------|----------------------|---------------------|
| `DJI-MAVIC-3-ABCD` | DJI | Mavic |
| `MAVIC3PRO` | DJI | Mavic 3 Pro |
| `MINI-5-PRO-XYZ` | DJI | Mini 5 Pro |
| `ANAFI-AI-001` | Parrot | Anafi |
| `EVO-III-PRO` | Autel | EVO III Pro |
| `SKYDIO-X10D` | Skydio | Skydio X10D |
| `TELLO-123456` | DJI/Ryze | Tello |
| `dji-air-3s` | DJI | Air |

**Injection Command**:
```go
det := &pb.Detection{
    TapId:        "tap-001",
    MacAddress:   "00:11:22:33:44:55", // Unknown OUI
    Ssid:         "DJI-MAVIC-3-ABCD",
    Rssi:         -60,
    Channel:      36,
    Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
    Manufacturer: "UNKNOWN",
    Designation:  "SUSPECT",
    Confidence:   0.40,
}
```

### 5.3 Expected Results / Assertions

**For SSID `DJI-MAVIC-3-ABCD`**:
```json
{
  "identifier": "SUSPECT-00:11:22:33:44:55",
  "ssid": "DJI-MAVIC-3-ABCD",
  "manufacturer": "DJI",
  "model": "",
  "designation": "Mavic",
  "detection_source": "WIFI_CORRELATED"
}
```

**Validation Checklist**:
- [ ] `MatchSSID()` returns non-nil for all patterns
- [ ] Manufacturer correctly identified
- [ ] Model or ModelHint populated
- [ ] Case-insensitive matching works (lowercase `dji-air-3s`)
- [ ] Controller patterns (`DJI-RC`) set `IsController=true`

---

## Test 6: False Positive Prevention

### 6.1 High-Level Test Concept

- Validates that non-drone WiFi devices are NOT promoted
- Tests rejection of router/AP SSIDs
- Confirms SERCOMM, Netgear, and other router OUIs are ignored
- Ensures channel/RSSI alone never triggers drone detection

### 6.2 Test Design & Steps

**False Positive Test Cases**:

| MAC OUI | SSID | Expected |
|---------|------|----------|
| `00:1E:58:AA:BB:CC` (SERCOMM) | `NETGEAR-5G` | REJECT |
| `F8:1A:67:AA:BB:CC` (TP-Link) | `TP-Link_5G_Home` | REJECT |
| `24:0A:C4:AA:BB:CC` (ESP32) | `ESP_DRONE_001` | ACCEPT (known pattern) |
| `24:0A:C4:AA:BB:CC` (ESP32) | `MyHomeESP` | REJECT |

**Router Beacon Injection**:
```go
det := &pb.Detection{
    TapId:        "tap-001",
    MacAddress:   "00:1E:58:AA:BB:CC", // SERCOMM OUI
    Ssid:         "NETGEAR-5G-HOME",
    Rssi:         -55,
    Channel:      149,
    Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
    Manufacturer: "UNKNOWN",
    Designation:  "SUSPECT",
}
```

### 6.3 Expected Results / Assertions

**After Injection** (`GET /api/drones`):
```json
{
  "drones": []
}
```

**After Injection** (`GET /api/suspects`):
- Should NOT contain SERCOMM MAC
- Router detections should be silently dropped

**Validation Checklist**:
- [ ] SERCOMM OUI `00:1E:58` not in `ouiMap`
- [ ] No drone match for `MatchOUI("00:1E:58:...")`
- [ ] No SSID match for `MatchSSID("NETGEAR-5G")`
- [ ] Detection dropped before reaching suspect tracking
- [ ] ESP32 OUI with generic SSID rejected
- [ ] ESP32 OUI with `ESP_DRONE` pattern accepted

---

## Test 7: Multi-TAP Correlation

### 7.1 High-Level Test Concept

- Validates that same MAC seen by 2+ TAPs triggers promotion
- Tests correlation window (30 seconds default)
- Confirms confidence boost per additional TAP
- Measures correlation latency

### 7.2 Test Design & Steps

**Scenario Timeline**:
```
T=0.0s:  TAP-001 sees MAC 60:60:1F:AA:BB:CC, RSSI=-65, Channel=149
T=2.0s:  TAP-002 sees MAC 60:60:1F:AA:BB:CC, RSSI=-72, Channel=149
T=4.0s:  TAP-003 sees MAC 60:60:1F:AA:BB:CC, RSSI=-80, Channel=149

Expected: Promotion after TAP-002 (T=2.0s)
Expected: Confidence boost after TAP-003 (T=4.0s)
```

**Injection Script**:
```bash
#!/bin/bash
BASE_PAYLOAD='{"tap_id":"tap-001","mac_address":"60:60:1F:AA:BB:CC",...}'

# TAP 1
echo "$BASE_PAYLOAD" | sed 's/tap-001/tap-001/' | nats pub skylens.detections.tap-001
echo "T=0: TAP-001 detection sent"

sleep 2

# TAP 2 (should trigger promotion)
echo "$BASE_PAYLOAD" | sed 's/tap-001/tap-002/' | nats pub skylens.detections.tap-002
echo "T=2: TAP-002 detection sent"

sleep 0.5

# Verify promotion
curl -s http://localhost:8080/api/drones | jq '.drones[] | select(.mac=="60:60:1F:AA:BB:CC")'

sleep 2

# TAP 3 (should boost confidence)
echo "$BASE_PAYLOAD" | sed 's/tap-001/tap-003/' | nats pub skylens.detections.tap-003
echo "T=4: TAP-003 detection sent"

# Final verification
curl -s http://localhost:8080/api/drones | jq '.drones[] | select(.mac=="60:60:1F:AA:BB:CC")'
```

### 7.3 Expected Results / Assertions

**After 2 TAPs**:
```json
{
  "detection_source": "MULTI_TAP_CORRELATED",
  "trust_score": 70,
  "confidence": 0.65
}
```

**After 3 TAPs**:
```json
{
  "detection_source": "MULTI_TAP_CORRELATED",
  "trust_score": 75,
  "confidence": 0.95
}
```

**Validation Checklist**:
- [ ] Promotion occurs after 2nd TAP (not before)
- [ ] Combined confidence = base + (taps-1) * 0.30
- [ ] Confidence capped at 0.95
- [ ] Correlation happens within correlation window (30s)
- [ ] Stale observations (>30s old) are not counted

---

## Test 8: End-to-End Latency

### 8.1 High-Level Test Concept

- Measures complete detection pipeline latency
- Validates <100ms target from TAP capture to WebSocket broadcast
- Identifies bottlenecks in processing chain

### 8.2 Test Design & Steps

**Instrumentation Points**:
```
[TAP Capture] --> [NATS Publish] --> [Node Subscribe] --> [Parse]
    T1              T2                  T3                 T4

--> [Spoof Check] --> [State Update] --> [WebSocket Broadcast] --> [API Available]
        T5                T6                    T7                     T8
```

**Latency Test Script**:
```go
package main

import (
    "fmt"
    "time"
    "sync"

    "github.com/nats-io/nats.go"
    "github.com/gorilla/websocket"
)

func main() {
    // Connect to WebSocket
    ws, _, _ := websocket.DefaultDialer.Dial("ws://localhost:8081/ws", nil)

    var received sync.WaitGroup
    var wsReceiveTime time.Time

    // Listen for WebSocket message
    received.Add(1)
    go func() {
        defer received.Done()
        for {
            _, msg, err := ws.ReadMessage()
            if err != nil {
                return
            }
            if bytes.Contains(msg, []byte("TEST-LATENCY-MAC")) {
                wsReceiveTime = time.Now()
                return
            }
        }
    }()

    // Connect to NATS
    nc, _ := nats.Connect("nats://localhost:4222")

    // Build detection message
    det := buildTestDetection("TEST-LATENCY-MAC")
    payload, _ := proto.Marshal(det)

    // Measure
    publishTime := time.Now()
    nc.Publish("skylens.detections.tap-001", payload)

    // Wait for WebSocket
    received.Wait()

    latency := wsReceiveTime.Sub(publishTime)
    fmt.Printf("End-to-end latency: %v\n", latency)

    if latency > 100*time.Millisecond {
        fmt.Printf("FAIL: Latency exceeds 100ms target\n")
    } else {
        fmt.Printf("PASS: Latency within target\n")
    }
}
```

### 8.3 Expected Results / Assertions

**Latency Breakdown Targets**:
| Stage | Target | Metric |
|-------|--------|--------|
| NATS Publish | <5ms | Network RTT |
| Node Subscribe | <2ms | Queue depth |
| Protobuf Parse | <1ms | CPU |
| Intel Engine | <10ms | Fingerprinting |
| Spoof Detection | <5ms | History lookup |
| State Update | <5ms | Lock contention |
| DB Save (async) | N/A | Background |
| WebSocket Broadcast | <10ms | Fan-out |
| **Total** | **<100ms** | **E2E** |

**Validation Checklist**:
- [ ] 95th percentile latency < 100ms
- [ ] No message drops under normal load
- [ ] Latency does not degrade with drone count
- [ ] Database saves do not block main path

---

## Test Harness Implementation

### Go Test File: `test/detection_pipeline_test.go`

```go
package test

import (
    "context"
    "encoding/binary"
    "net/http"
    "testing"
    "time"

    "github.com/nats-io/nats.go"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    "google.golang.org/protobuf/proto"

    pb "github.com/skylens/skylens-node/proto"
    "github.com/skylens/skylens-node/internal/intel"
)

const (
    natsURL    = "nats://localhost:4222"
    apiBaseURL = "http://localhost:8080"
    wsURL      = "ws://localhost:8081/ws"
)

// TestRemoteIDDetection validates ASTM F3411 RemoteID parsing
func TestRemoteIDDetection(t *testing.T) {
    nc, err := nats.Connect(natsURL)
    require.NoError(t, err)
    defer nc.Close()

    // Build RemoteID payload
    payload := buildASTMRemoteIDPayload(t,
        "1581F5FKD229400000",  // Serial
        37.7749, -122.4194,    // Position
        120.0, 8.5, 180.0,     // Alt, Speed, Heading
        "FAA-PILOT-123456",    // Operator ID
    )

    det := &pb.Detection{
        TapId:           "tap-001",
        TimestampNs:     time.Now().UnixNano(),
        MacAddress:      "60:60:1F:AA:BB:CC",
        Identifier:      "test-remoteid-001",
        Rssi:            -55,
        Channel:         149,
        Source:          pb.DetectionSource_SOURCE_WIFI_NAN,
        RemoteidPayload: payload,
    }

    data, err := proto.Marshal(det)
    require.NoError(t, err)

    err = nc.Publish("skylens.detections.tap-001", data)
    require.NoError(t, err)

    // Wait for processing
    time.Sleep(100 * time.Millisecond)

    // Verify via API
    resp, err := http.Get(apiBaseURL + "/api/drones")
    require.NoError(t, err)
    defer resp.Body.Close()

    var result struct {
        Drones []map[string]interface{} `json:"drones"`
    }
    err = json.NewDecoder(resp.Body).Decode(&result)
    require.NoError(t, err)

    // Find our drone
    var drone map[string]interface{}
    for _, d := range result.Drones {
        if d["mac"] == "60:60:1F:AA:BB:CC" {
            drone = d
            break
        }
    }
    require.NotNil(t, drone, "Drone not found in API response")

    // Validate fields
    assert.Equal(t, "1581F5FKD229400000", drone["serial_number"])
    assert.Equal(t, "Mavic 3", drone["model"])
    assert.InDelta(t, 37.7749, drone["latitude"].(float64), 0.001)
    assert.InDelta(t, -122.4194, drone["longitude"].(float64), 0.001)
    assert.InDelta(t, 120.0, drone["altitude_geodetic"].(float64), 5.0)
    assert.InDelta(t, 8.5, drone["speed"].(float64), 1.0)
    assert.Equal(t, "FAA-PILOT-123456", drone["operator_id"])
    assert.GreaterOrEqual(t, int(drone["trust_score"].(float64)), 85)
}

// TestDJIDroneIDDetection validates DJI OcuSync DroneID parsing
func TestDJIDroneIDDetection(t *testing.T) {
    nc, err := nats.Connect(natsURL)
    require.NoError(t, err)
    defer nc.Close()

    // Build DJI DroneID payload
    payload := buildDJIDroneIDPayload(t,
        "1581F44KD234500123",  // Serial
        37.7749, -122.4194,    // Position
        150.0, 80.0,           // Alt, HeightAGL
        5.0, 3.0, -1.0,        // Velocity N/E/D
        270.0,                 // Yaw
        0x44,                  // Product type: Mini 3 Pro
    )

    det := &pb.Detection{
        TapId:           "tap-001",
        TimestampNs:     time.Now().UnixNano(),
        MacAddress:      "60:60:1F:BB:CC:DD",
        Identifier:      "test-dji-001",
        Rssi:            -55,
        Channel:         149,
        Source:          pb.DetectionSource_SOURCE_DJI_OCUSYNC,
        RemoteidPayload: payload,
    }

    data, err := proto.Marshal(det)
    require.NoError(t, err)

    err = nc.Publish("skylens.detections.tap-001", data)
    require.NoError(t, err)

    time.Sleep(100 * time.Millisecond)

    // Verify via API
    drone := getDroneByMAC(t, "60:60:1F:BB:CC:DD")
    require.NotNil(t, drone)

    assert.Equal(t, "1581F44KD234500123", drone["serial_number"])
    assert.Equal(t, "Mini 3 Pro", drone["model"])
    assert.Equal(t, "DJI", drone["manufacturer"])
    assert.InDelta(t, 270.0, drone["heading"].(float64), 5.0)
}

// TestFalsePositivePrevention validates router rejection
func TestFalsePositivePrevention(t *testing.T) {
    nc, err := nats.Connect(natsURL)
    require.NoError(t, err)
    defer nc.Close()

    testCases := []struct {
        name     string
        mac      string
        ssid     string
        shouldReject bool
    }{
        {"SERCOMM router", "00:1E:58:AA:BB:CC", "NETGEAR-5G", true},
        {"TP-Link router", "F8:1A:67:AA:BB:CC", "TP-Link_Home", true},
        {"ESP32 generic", "24:0A:C4:AA:BB:CC", "MyHomeESP", true},
        {"ESP32 drone", "24:0A:C4:AA:BB:CC", "ESP_DRONE_001", false},
        {"DJI OUI", "60:60:1F:AA:BB:CC", "", false},
    }

    for _, tc := range testCases {
        t.Run(tc.name, func(t *testing.T) {
            // Clear previous state
            http.Post(apiBaseURL + "/api/test/clear", "", nil)

            det := &pb.Detection{
                TapId:        "tap-001",
                MacAddress:   tc.mac,
                Ssid:         tc.ssid,
                Rssi:         -60,
                Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
                Manufacturer: "UNKNOWN",
                Designation:  "SUSPECT",
            }

            data, _ := proto.Marshal(det)
            nc.Publish("skylens.detections.tap-001", data)
            time.Sleep(50 * time.Millisecond)

            // Check if promoted
            drone := getDroneByMAC(t, tc.mac)
            suspect := getSuspectByMAC(t, tc.mac)

            if tc.shouldReject {
                assert.Nil(t, drone, "Router should not be promoted to drone")
                // Routers should not even be tracked as suspects
                // (unless they have drone OUI)
            } else {
                // Should be tracked as suspect or drone
                assert.True(t, drone != nil || suspect != nil,
                    "Drone signal should be tracked")
            }
        })
    }
}

// TestMultiTAPCorrelation validates multi-TAP promotion
func TestMultiTAPCorrelation(t *testing.T) {
    nc, err := nats.Connect(natsURL)
    require.NoError(t, err)
    defer nc.Close()

    // Clear previous state
    http.Post(apiBaseURL + "/api/test/clear", "", nil)

    mac := "60:60:1F:CC:DD:EE"

    // TAP 1 detection
    det1 := &pb.Detection{
        TapId:        "tap-001",
        MacAddress:   mac,
        Rssi:         -65,
        Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
        Manufacturer: "UNKNOWN",
        Designation:  "SUSPECT",
        Confidence:   0.35,
    }
    data1, _ := proto.Marshal(det1)
    nc.Publish("skylens.detections.tap-001", data1)

    time.Sleep(50 * time.Millisecond)

    // Should be suspect, not drone
    drone := getDroneByMAC(t, mac)
    assert.Nil(t, drone, "Single TAP should not promote to drone")

    suspect := getSuspectByMAC(t, mac)
    assert.NotNil(t, suspect, "Should be tracked as suspect")

    // TAP 2 detection (should trigger promotion)
    det2 := &pb.Detection{
        TapId:        "tap-002",
        MacAddress:   mac,
        Rssi:         -72,
        Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
        Manufacturer: "UNKNOWN",
        Designation:  "SUSPECT",
        Confidence:   0.35,
    }
    data2, _ := proto.Marshal(det2)
    nc.Publish("skylens.detections.tap-002", data2)

    time.Sleep(100 * time.Millisecond)

    // Should now be promoted to drone
    drone = getDroneByMAC(t, mac)
    require.NotNil(t, drone, "Two TAPs should promote to drone")
    assert.Equal(t, "MULTI_TAP_CORRELATED", drone["detection_source"])
    assert.GreaterOrEqual(t, int(drone["trust_score"].(float64)), 60)
}

// TestEndToEndLatency measures detection pipeline latency
func TestEndToEndLatency(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping latency test in short mode")
    }

    nc, err := nats.Connect(natsURL)
    require.NoError(t, err)
    defer nc.Close()

    // Connect to WebSocket
    ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
    require.NoError(t, err)
    defer ws.Close()

    latencies := make([]time.Duration, 0, 100)

    for i := 0; i < 100; i++ {
        mac := fmt.Sprintf("60:60:1F:%02X:%02X:%02X", i, i, i)

        // Prepare detection
        det := &pb.Detection{
            TapId:           "tap-001",
            MacAddress:      mac,
            SerialNumber:    fmt.Sprintf("1581F5FK%08d", i),
            Latitude:        37.7749,
            Longitude:       -122.4194,
            Source:          pb.DetectionSource_SOURCE_WIFI_NAN,
        }
        data, _ := proto.Marshal(det)

        // Start timer
        start := time.Now()

        // Publish
        nc.Publish("skylens.detections.tap-001", data)

        // Wait for WebSocket message
        ws.SetReadDeadline(time.Now().Add(5 * time.Second))
        for {
            _, msg, err := ws.ReadMessage()
            if err != nil {
                t.Fatalf("WebSocket read error: %v", err)
            }
            if bytes.Contains(msg, []byte(mac)) {
                break
            }
        }

        latency := time.Since(start)
        latencies = append(latencies, latency)
    }

    // Calculate percentiles
    sort.Slice(latencies, func(i, j int) bool {
        return latencies[i] < latencies[j]
    })

    p50 := latencies[50]
    p95 := latencies[95]
    p99 := latencies[99]

    t.Logf("Latency P50: %v, P95: %v, P99: %v", p50, p95, p99)

    assert.Less(t, p95, 100*time.Millisecond,
        "95th percentile latency should be under 100ms")
}

// Helper functions

func buildASTMRemoteIDPayload(t *testing.T, serial string, lat, lon, alt, speed, heading float64, operatorID string) []byte {
    // Implementation as described in test section 1.2
    // ...
    return payload
}

func buildDJIDroneIDPayload(t *testing.T, serial string, lat, lon, alt, heightAGL, vN, vE, vD, yaw float64, productType byte) []byte {
    // Implementation as described in test section 2.2
    // ...
    return payload
}

func getDroneByMAC(t *testing.T, mac string) map[string]interface{} {
    resp, err := http.Get(apiBaseURL + "/api/drones")
    require.NoError(t, err)
    defer resp.Body.Close()

    var result struct {
        Drones []map[string]interface{} `json:"drones"`
    }
    json.NewDecoder(resp.Body).Decode(&result)

    for _, d := range result.Drones {
        if d["mac"] == mac {
            return d
        }
    }
    return nil
}

func getSuspectByMAC(t *testing.T, mac string) map[string]interface{} {
    resp, err := http.Get(apiBaseURL + "/api/suspects")
    require.NoError(t, err)
    defer resp.Body.Close()

    var result struct {
        Suspects []map[string]interface{} `json:"suspects"`
    }
    json.NewDecoder(resp.Body).Decode(&result)

    for _, s := range result.Suspects {
        if s["mac_address"] == mac {
            return s
        }
    }
    return nil
}
```

---

## Continuous Integration

### GitHub Actions Workflow: `.github/workflows/detection-tests.yml`

```yaml
name: Detection Pipeline Tests

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  detection-tests:
    runs-on: ubuntu-latest

    services:
      nats:
        image: nats:latest
        ports:
          - 4222:4222

      postgres:
        image: postgres:15
        env:
          POSTGRES_DB: skylens_test
          POSTGRES_USER: skylens
          POSTGRES_PASSWORD: test
        ports:
          - 5432:5432

      redis:
        image: redis:7
        ports:
          - 6379:6379

    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'

      - name: Build skylens-node
        run: go build -o skylens-node ./cmd/skylens-node

      - name: Start skylens-node
        run: |
          ./skylens-node --config=test/config.test.yaml &
          sleep 5

      - name: Run Detection Tests
        run: |
          go test -v -timeout 5m ./test/...

      - name: Run Latency Tests
        run: |
          go test -v -run TestEndToEndLatency ./test/...
```

---

## Summary: Validation Criteria

| Test | Pass Criteria |
|------|---------------|
| 1. RemoteID | Serial, position, operator_id extracted |
| 2. DJI OcuSync | SubCmd 0x13 parsed, model from product type |
| 3. DJI Legacy | Legacy OUI recognized, same parsing quality |
| 4. OUI-Only | Requires multi-TAP or mobility for promotion |
| 5. SSID Pattern | Manufacturer and model hint extracted |
| 6. False Positive | Routers NOT promoted |
| 7. Multi-TAP | Promotion after 2+ TAPs, confidence boost |
| 8. Latency | P95 < 100ms |

---

## Files Referenced

- `/home/node/skylens-node/proto/skylens.proto` - Protobuf definitions
- `/home/node/skylens-node/internal/intel/remoteid_parser.go` - ASTM F3411 parsing
- `/home/node/skylens-node/internal/intel/dji_parser.go` - DJI DroneID parsing
- `/home/node/skylens-node/internal/intel/fingerprint.go` - OUI/SSID matching
- `/home/node/skylens-node/internal/receiver/nats.go` - Detection processing
- `/home/node/skylens-node/internal/processor/suspect_correlator.go` - Multi-TAP correlation
- `/home/node/skylens-node/internal/processor/spoof.go` - Spoof detection

---

*Generated by Skylens Simulation & QA Engineer*
*Version: 1.0.0*
*Date: 2026-02-09*
