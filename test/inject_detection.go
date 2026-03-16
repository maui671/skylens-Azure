// +build ignore

// inject_detection.go - CLI tool for injecting test detections
//
// Usage:
//   go run test/inject_detection.go [options]
//
// Examples:
//   # Inject ASTM F3411 RemoteID detection
//   go run test/inject_detection.go --type=remoteid \
//     --serial=1581F5FKD229400000 \
//     --lat=37.7749 --lon=-122.4194 --alt=120.0 \
//     --speed=8.5 --heading=180.0 \
//     --operator-id=FAA-PILOT-123456
//
//   # Inject DJI OcuSync detection
//   go run test/inject_detection.go --type=dji \
//     --serial=1581F44KD234500123 \
//     --lat=37.7749 --lon=-122.4194 --alt=150.0 \
//     --product-type=0x44
//
//   # Inject suspect (OUI-only) detection
//   go run test/inject_detection.go --type=suspect \
//     --mac=60:60:1F:AA:BB:CC \
//     --rssi=-65 --channel=149
//
//   # Inject false positive test (router)
//   go run test/inject_detection.go --type=router \
//     --mac=00:1E:58:AA:BB:CC \
//     --ssid=NETGEAR-5G

package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	pb "github.com/K13094/skylens/proto"
)

const djiCoordFactor = 174533.0

var (
	// Connection
	natsURL = flag.String("nats", "nats://localhost:4222", "NATS server URL")
	tapID   = flag.String("tap", "tap-001", "TAP ID")

	// Detection type
	detectionType = flag.String("type", "remoteid", "Detection type: remoteid, dji, legacy, suspect, router")

	// Common fields
	mac     = flag.String("mac", "", "MAC address (auto-generated if empty)")
	serial  = flag.String("serial", "", "Serial number")
	lat     = flag.Float64("lat", 37.7749, "Latitude")
	lon     = flag.Float64("lon", -122.4194, "Longitude")
	alt     = flag.Float64("alt", 120.0, "Altitude (meters)")
	speed   = flag.Float64("speed", 8.5, "Ground speed (m/s)")
	heading = flag.Float64("heading", 180.0, "Heading (degrees)")
	rssi    = flag.Int("rssi", -55, "RSSI (dBm)")
	channel = flag.Int("channel", 149, "WiFi channel")
	ssid    = flag.String("ssid", "", "SSID")

	// RemoteID specific
	operatorID  = flag.String("operator-id", "", "Operator ID")
	operatorLat = flag.Float64("operator-lat", 0, "Operator latitude")
	operatorLon = flag.Float64("operator-lon", 0, "Operator longitude")

	// DJI specific
	productType = flag.String("product-type", "0x44", "DJI product type (hex)")
	heightAGL   = flag.Float64("height-agl", 80.0, "Height above ground (meters)")

	// Multi-injection
	count    = flag.Int("count", 1, "Number of detections to inject")
	interval = flag.Duration("interval", time.Second, "Interval between detections")
	multiTap = flag.Bool("multi-tap", false, "Inject from multiple TAPs")
)

func main() {
	flag.Parse()

	// Connect to NATS
	nc, err := nats.Connect(*natsURL)
	if err != nil {
		log.Fatalf("Failed to connect to NATS: %v", err)
	}
	defer nc.Close()

	fmt.Printf("Connected to NATS at %s\n", *natsURL)

	// Generate MAC if not provided
	if *mac == "" {
		*mac = generateMAC(*detectionType)
	}

	// Generate serial if not provided
	if *serial == "" && (*detectionType == "remoteid" || *detectionType == "dji") {
		*serial = generateSerial()
	}

	// Inject detections
	for i := 0; i < *count; i++ {
		currentTap := *tapID
		if *multiTap && i > 0 {
			currentTap = fmt.Sprintf("tap-%03d", (i%3)+1)
		}

		det := buildDetection(currentTap, i)
		if det == nil {
			log.Fatalf("Failed to build detection")
		}

		data, err := proto.Marshal(det)
		if err != nil {
			log.Fatalf("Failed to marshal detection: %v", err)
		}

		subject := fmt.Sprintf("skylens.detections.%s", currentTap)
		if err := nc.Publish(subject, data); err != nil {
			log.Fatalf("Failed to publish: %v", err)
		}

		fmt.Printf("[%d/%d] Published %s detection: MAC=%s, Serial=%s, Lat=%.4f, Lon=%.4f\n",
			i+1, *count, *detectionType, det.MacAddress, det.SerialNumber, det.Latitude, det.Longitude)

		if i < *count-1 {
			time.Sleep(*interval)
		}
	}

	fmt.Println("Done.")
}

func buildDetection(tapID string, iteration int) *pb.Detection {
	switch *detectionType {
	case "remoteid":
		return buildRemoteIDDetection(tapID, iteration)
	case "dji":
		return buildDJIDetection(tapID, iteration)
	case "legacy":
		return buildLegacyDetection(tapID, iteration)
	case "suspect":
		return buildSuspectDetection(tapID, iteration)
	case "router":
		return buildRouterDetection(tapID, iteration)
	default:
		log.Fatalf("Unknown detection type: %s", *detectionType)
		return nil
	}
}

func buildRemoteIDDetection(tapID string, iteration int) *pb.Detection {
	payload := buildASTMRemoteIDPayload()

	opLat := *operatorLat
	opLon := *operatorLon
	if opLat == 0 {
		opLat = *lat - 0.001
	}
	if opLon == 0 {
		opLon = *lon + 0.001
	}

	return &pb.Detection{
		TapId:             tapID,
		TimestampNs:       time.Now().UnixNano(),
		MacAddress:        *mac,
		Identifier:        "REMOTEID-" + *serial,
		SerialNumber:      *serial,
		Latitude:          *lat + float64(iteration)*0.0001,
		Longitude:         *lon,
		AltitudeGeodetic:  float32(*alt),
		Speed:             float32(*speed),
		Heading:           float32(*heading),
		OperatorLatitude:  opLat,
		OperatorLongitude: opLon,
		OperatorId:        *operatorID,
		Rssi:              int32(*rssi),
		Channel:           int32(*channel),
		Source:            pb.DetectionSource_SOURCE_WIFI_NAN,
		Manufacturer:      "DJI",
		Designation:       getModelFromSerial(*serial),
		Confidence:        0.95,
		RemoteidPayload:   payload,
	}
}

func buildDJIDetection(tapID string, iteration int) *pb.Detection {
	pt := parseProductType(*productType)
	payload := buildDJIDroneIDPayload(pt)

	return &pb.Detection{
		TapId:           tapID,
		TimestampNs:     time.Now().UnixNano(),
		MacAddress:      *mac,
		Identifier:      "DJI-" + *mac,
		SerialNumber:    *serial,
		Latitude:        *lat + float64(iteration)*0.0001,
		Longitude:       *lon,
		Rssi:            int32(*rssi),
		Channel:         int32(*channel),
		Source:          pb.DetectionSource_SOURCE_DJI_OCUSYNC,
		Manufacturer:    "DJI",
		RemoteidPayload: payload,
	}
}

func buildLegacyDetection(tapID string, iteration int) *pb.Detection {
	pt := parseProductType(*productType)
	payload := buildDJILegacyPayload(pt)

	return &pb.Detection{
		TapId:           tapID,
		TimestampNs:     time.Now().UnixNano(),
		MacAddress:      *mac,
		Identifier:      "DJI-LEGACY-" + *mac,
		SerialNumber:    *serial,
		Latitude:        *lat + float64(iteration)*0.0001,
		Longitude:       *lon,
		Rssi:            int32(*rssi),
		Channel:         int32(*channel),
		Source:          pb.DetectionSource_SOURCE_WIFI_BEACON,
		RemoteidPayload: payload,
	}
}

func buildSuspectDetection(tapID string, iteration int) *pb.Detection {
	ssidVal := *ssid
	if ssidVal == "" && strings.HasPrefix(*mac, "60:60:1F") {
		ssidVal = "DJI-UNKNOWN"
	}

	return &pb.Detection{
		TapId:        tapID,
		TimestampNs:  time.Now().UnixNano(),
		MacAddress:   *mac,
		Identifier:   "SUSPECT-" + *mac,
		Ssid:         ssidVal,
		Rssi:         int32(*rssi + iteration*2), // Vary RSSI slightly
		Channel:      int32(*channel),
		Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
		Manufacturer: "UNKNOWN",
		Designation:  "SUSPECT",
		Confidence:   0.35,
	}
}

func buildRouterDetection(tapID string, iteration int) *pb.Detection {
	return &pb.Detection{
		TapId:        tapID,
		TimestampNs:  time.Now().UnixNano(),
		MacAddress:   *mac,
		Identifier:   "SUSPECT-" + *mac,
		Ssid:         *ssid,
		Rssi:         int32(*rssi),
		Channel:      int32(*channel),
		Source:       pb.DetectionSource_SOURCE_WIFI_BEACON,
		Manufacturer: "UNKNOWN",
		Designation:  "SUSPECT",
		Confidence:   0.35,
	}
}

func buildASTMRemoteIDPayload() []byte {
	// MessagePack: 4 messages of 25 bytes each
	result := []byte{0xF0, 25, 4}

	// BasicID
	basicID := make([]byte, 25)
	basicID[0] = 0x00 // Type 0, Version 0
	basicID[1] = 0x12 // SerialNumber, Helicopter
	copy(basicID[2:22], []byte(*serial))
	result = append(result, basicID...)

	// Location
	location := make([]byte, 25)
	location[0] = 0x10
	location[1] = 0x2C
	if *heading >= 0 && *heading < 360 {
		location[2] = byte(*heading / 2)
	}
	location[3] = byte(*speed * 4)
	location[5] = 63

	latScaled := int32(*lat * 1e7)
	binary.LittleEndian.PutUint32(location[6:10], uint32(latScaled))
	lonScaled := int32(*lon * 1e7)
	binary.LittleEndian.PutUint32(location[10:14], uint32(lonScaled))
	altScaled := uint16((*alt + 1000) * 2)
	binary.LittleEndian.PutUint16(location[14:16], altScaled)
	binary.LittleEndian.PutUint16(location[16:18], altScaled)
	binary.LittleEndian.PutUint16(location[18:20], altScaled)
	result = append(result, location...)

	// System
	system := make([]byte, 25)
	system[0] = 0x40
	system[1] = 0x01
	if *operatorLat != 0 {
		opLatScaled := int32(*operatorLat * 1e7)
		binary.LittleEndian.PutUint32(system[3:7], uint32(opLatScaled))
	}
	if *operatorLon != 0 {
		opLonScaled := int32(*operatorLon * 1e7)
		binary.LittleEndian.PutUint32(system[7:11], uint32(opLonScaled))
	}
	result = append(result, system...)

	// OperatorID
	opIDMsg := make([]byte, 25)
	opIDMsg[0] = 0x50
	if *operatorID != "" {
		copy(opIDMsg[2:22], []byte(*operatorID))
	}
	result = append(result, opIDMsg...)

	return result
}

func buildDJIDroneIDPayload(productType byte) []byte {
	payload := make([]byte, 88)

	// OcuSync OUI
	payload[0] = 0x60
	payload[1] = 0x60
	payload[2] = 0x1F
	payload[3] = 0x13 // SubCmd

	droneID := payload[4:]
	droneID[0] = 0x02
	binary.LittleEndian.PutUint16(droneID[1:3], uint16(rand.Intn(65535)))
	droneID[3] = 0x0F
	droneID[4] = 0x00
	copy(droneID[5:21], []byte(*serial))

	lonRaw := int32(*lon * djiCoordFactor * math.Pi / 180.0)
	binary.LittleEndian.PutUint32(droneID[21:25], uint32(lonRaw))
	latRaw := int32(*lat * djiCoordFactor * math.Pi / 180.0)
	binary.LittleEndian.PutUint32(droneID[25:29], uint32(latRaw))

	binary.LittleEndian.PutUint16(droneID[29:31], uint16(*alt))
	binary.LittleEndian.PutUint16(droneID[31:33], uint16(*heightAGL))

	// Velocity
	vN := int16(*speed * math.Cos(*heading*math.Pi/180.0) * 100)
	vE := int16(*speed * math.Sin(*heading*math.Pi/180.0) * 100)
	binary.LittleEndian.PutUint16(droneID[33:35], uint16(vN))
	binary.LittleEndian.PutUint16(droneID[35:37], uint16(vE))

	binary.LittleEndian.PutUint16(droneID[43:45], uint16(*heading*100))
	droneID[53] = productType

	return payload
}

func buildDJILegacyPayload(productType byte) []byte {
	payload := buildDJIDroneIDPayload(productType)
	// Replace OUI with legacy
	payload[0] = 0x26
	payload[1] = 0x37
	payload[2] = 0x12
	return payload
}

func generateMAC(detType string) string {
	switch detType {
	case "remoteid", "dji", "legacy":
		return fmt.Sprintf("60:60:1F:%02X:%02X:%02X",
			rand.Intn(256), rand.Intn(256), rand.Intn(256))
	case "suspect":
		return fmt.Sprintf("60:60:1F:%02X:%02X:%02X",
			rand.Intn(256), rand.Intn(256), rand.Intn(256))
	case "router":
		// Use SERCOMM OUI
		return fmt.Sprintf("00:1E:58:%02X:%02X:%02X",
			rand.Intn(256), rand.Intn(256), rand.Intn(256))
	default:
		return fmt.Sprintf("00:11:22:%02X:%02X:%02X",
			rand.Intn(256), rand.Intn(256), rand.Intn(256))
	}
}

func generateSerial() string {
	prefixes := []string{"1581", "531", "1SS", "3NZ"}
	codes := []string{"F5F", "F44", "F5G", "F5K", "F5C", "F5A"}
	prefix := prefixes[rand.Intn(len(prefixes))]
	code := codes[rand.Intn(len(codes))]
	suffix := fmt.Sprintf("%010d", rand.Intn(10000000000))
	return prefix + code + suffix[:10]
}

func getModelFromSerial(serial string) string {
	if len(serial) < 7 {
		return "Unknown"
	}
	code := serial[4:7]
	models := map[string]string{
		"F5F": "Mavic 3",
		"F5J": "Mavic 3 Pro",
		"F44": "Mini 3 Pro",
		"F45": "Mini 4 Pro",
		"F5G": "Mini 3 Pro",
		"F5C": "Air 3",
		"F5A": "Air 2S",
		"F5D": "Avata",
		"F4Q": "Mini 2",
	}
	if model, ok := models[code]; ok {
		return model
	}
	return "Unknown DJI"
}

func parseProductType(s string) byte {
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	val, err := strconv.ParseUint(s, 16, 8)
	if err != nil {
		return 0x44 // Default to Mini 3 Pro
	}
	return byte(val)
}

func init() {
	rand.Seed(time.Now().UnixNano())
}
