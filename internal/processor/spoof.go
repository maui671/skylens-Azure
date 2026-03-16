package processor

import (
	"math"
	"strings"
	"sync"
	"time"

	"github.com/K13094/skylens/internal/intel"
)

// Number of shards for spoof detector - power of 2 for fast modulo
const numSpoofShards = 16

// spoofShard holds a subset of drone tracks with its own lock
type spoofShard struct {
	mu             sync.RWMutex
	tracks         map[string]*droneTrack
	identifierMACs map[string]map[string]*identifierMACInfo
}

// SpoofDetector analyzes drone data for anomalies
// Sharded to reduce lock contention under high detection rates
type SpoofDetector struct {
	shards [numSpoofShards]*spoofShard
}

type droneTrack struct {
	lastLat              float64
	lastLng              float64
	lastAlt              float32
	lastSpeed            float32
	lastTime             time.Time
	flags                map[string]time.Time
	detectionGaps        []time.Duration
	consecutiveClean     int       // Consecutive samples with no new flags
	lastRSSI             int32     // For RSSI trend analysis
	firstSeen            time.Time // When we first saw this drone
}

// identifierMACInfo tracks location info for an identifier-MAC pair
type identifierMACInfo struct {
	lastLat  float64
	lastLng  float64
	lastSeen time.Time
}

// Penalty points for various anomalies
var penalties = map[string]int{
	"coordinate_jump":        30, // Impossible teleportation
	"altitude_spike":         20, // Sudden altitude change
	"speed_violation":        25, // Exceeds physical limits
	"no_serial":              5,  // Missing serial (many consumer drones don't broadcast)
	"invalid_coordinates":    40, // Out of range lat/lng
	"timestamp_anomaly":      15, // Future or ancient timestamp
	"duplicate_id":           35, // Same ID from different locations
	"rssi_impossible":        20, // RSSI doesn't match distance
	"randomized_mac":         15, // Locally administered MAC (randomized)
	"rssi_distance_mismatch": 25, // Strong RSSI but claims to be far away
	"low_confidence":         15, // Detection confidence < 0.50
	"oui_ssid_mismatch":      35, // OUI vendor doesn't match SSID vendor (likely spoof)
	"impossible_vendor":      40, // Generic OUI (ESP32) claiming to be enterprise drone
}

// Bonus points for high-confidence detections
var bonuses = map[string]int{
	"high_confidence":   10, // confidence >= 0.90 (RemoteID with location)
	"medium_confidence": 5,  // confidence >= 0.80 (protocol decoded)
	// Dual/triple-signal correlation bonuses
	"oui_ssid_match":           10, // OUI vendor matches SSID vendor (no RemoteID)
	"oui_ssid_remoteid_match":  15, // OUI + SSID + RemoteID manufacturer all match
	"triple_match_with_serial": 20, // All three match AND has valid serial number
}

const (
	maxDroneSpeedMS      = 80.0  // ~180 mph, faster than most drones
	maxAltitudeChange    = 100.0 // meters per second
	earthRadiusKM        = 6371.0
	flagExpirySec        = 300   // Flags expire after 5 minutes
	trustRecoveryClean   = 10    // Consecutive clean samples for trust boost
	trustRecoveryAmount  = 5     // Points to recover per cycle
	maxTimestampDriftSec = 30    // Maximum acceptable timestamp drift
)

// Package-level static maps — allocated once, not per-detection call

// ssidVendorPatterns maps SSID keywords to vendor names for OUI-SSID consistency checks
var ssidVendorPatterns = map[string][]string{
	"DJI":              {"DJI", "MAVIC", "PHANTOM", "INSPIRE", "MATRICE", "MINI", "AIR", "AVATA", "FPV", "SPARK", "AGRAS", "TELLO", "NEO", "FLIP", "FLYCART"},
	"PARROT":           {"PARROT", "ANAFI", "BEBOP", "DISCO", "MAMBO", "SWING", "SKYCONTROLLER"},
	"AUTEL":            {"AUTEL", "EVO", "DRAGONFISH", "TITAN", "ALPHA"},
	"SKYDIO":           {"SKYDIO", "X2", "X10"},
	"YUNEEC":           {"YUNEEC", "TYPHOON", "MANTIS", "H520"},
	"HUBSAN":           {"HUBSAN", "ZINO"},
	"HOLY STONE":       {"HOLYSTONE", "HS720", "HS710", "HS175"},
	"FIMI":             {"FIMI", "X8SE", "X8MINI"},
	"GOPRO":            {"GOPRO", "KARMA"},
	"ESPRESSIF":        {"ESP-DRONE"},
	"POTENSIC":         {"POTENSIC", "DREAMER", "ATOM"},
	"RUKO":             {"RUKO", "F11", "U11"},
	"AGEAGLE":          {"EBEE", "AGEAGLE", "SENSEFLY"},
	"INSPIRED FLIGHT":  {"IF1200", "IF800", "INSPIRED"},
	"FREEFLY":          {"FREEFLY", "ALTA", "ASTRO"},
}

// vendorPrefixMap maps SSID prefixes to vendor names for extractVendorFromSSID
var vendorPrefixMap = map[string]string{
	"DJI":        "DJI",
	"MAVIC":      "DJI",
	"PHANTOM":    "DJI",
	"INSPIRE":    "DJI",
	"MATRICE":    "DJI",
	"MINI":       "DJI",
	"AIR":        "DJI",
	"AVATA":      "DJI",
	"FPV":        "DJI",
	"SPARK":      "DJI",
	"AGRAS":      "DJI",
	"TELLO":      "DJI",
	"NEO":        "DJI",
	"FLIP":       "DJI",
	"FLYCART":    "DJI",
	"ANAFI":      "PARROT",
	"BEBOP":      "PARROT",
	"PARROT":     "PARROT",
	"DISCO":      "PARROT",
	"SKYCONTROL": "PARROT",
	"AUTEL":      "AUTEL",
	"EVO":        "AUTEL",
	"DRAGONFISH": "AUTEL",
	"TITAN":      "AUTEL",
	"ALPHA":      "AUTEL",
	"SKYDIO":     "SKYDIO",
	"X10":        "SKYDIO",
	"X2":         "SKYDIO",
	"YUNEEC":     "YUNEEC",
	"TYPHOON":    "YUNEEC",
	"MANTIS":     "YUNEEC",
	"HUBSAN":     "HUBSAN",
	"ZINO":       "HUBSAN",
	"HOLYSTONE":  "HOLY STONE",
	"HS":         "HOLY STONE",
	"FIMI":       "FIMI",
	"X8SE":       "FIMI",
	"GOPRO":      "GOPRO",
	"KARMA":      "GOPRO",
	"HOVERAIR":   "ZERO ZERO",
	"POTENSIC":   "POTENSIC",
	"DREAMER":    "POTENSIC",
	"ATOM":       "POTENSIC",
	"RUKO":       "RUKO",
	"F11":        "RUKO",
	"U11":        "RUKO",
	"EBEE":       "AGEAGLE",
	"AGEAGLE":    "AGEAGLE",
	"SENSEFLY":   "AGEAGLE",
	"IF1200":     "INSPIRED FLIGHT",
	"IF800":      "INSPIRED FLIGHT",
	"FREEFLY":    "FREEFLY",
	"ALTA":       "FREEFLY",
	"ASTRO":      "FREEFLY",
}

// getShard returns the shard for a given identifier using inline FNV-1a (zero allocation)
func (s *SpoofDetector) getShard(identifier string) *spoofShard {
	var h uint32 = 2166136261 // FNV-1a offset basis
	for i := 0; i < len(identifier); i++ {
		h ^= uint32(identifier[i])
		h *= 16777619 // FNV-1a prime
	}
	return s.shards[h&(numSpoofShards-1)]
}

// NewSpoofDetector creates a new spoof detector
func NewSpoofDetector() *SpoofDetector {
	sd := &SpoofDetector{}
	for i := 0; i < numSpoofShards; i++ {
		sd.shards[i] = &spoofShard{
			tracks:         make(map[string]*droneTrack),
			identifierMACs: make(map[string]map[string]*identifierMACInfo),
		}
	}
	return sd
}

// modifyTrack provides test-safe access to modify a drone track's internal state.
// The callback runs under the shard's write lock.
func (s *SpoofDetector) modifyTrack(identifier string, fn func(*droneTrack)) {
	shard := s.getShard(identifier)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if track, ok := shard.tracks[identifier]; ok {
		fn(track)
	}
}

// trackCount returns the total number of tracks across all shards (for testing)
func (s *SpoofDetector) trackCount() int {
	total := 0
	for i := 0; i < numSpoofShards; i++ {
		shard := s.shards[i]
		shard.mu.RLock()
		total += len(shard.tracks)
		shard.mu.RUnlock()
	}
	return total
}

// Analyze checks a drone detection for anomalies
// Returns trust score (0-100) and list of flags
func (s *SpoofDetector) Analyze(d *Drone) (int, []string) {
	shard := s.getShard(d.Identifier)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	now := time.Now()
	flagsAddedThisSample := 0

	// Get or create track
	track, exists := shard.tracks[d.Identifier]
	if !exists {
		track = &droneTrack{
			flags:     make(map[string]time.Time),
			firstSeen: now,
		}
		shard.tracks[d.Identifier] = track
	}

	// Clean expired flags
	for flag, ts := range track.flags {
		if now.Sub(ts) > time.Duration(flagExpirySec)*time.Second {
			delete(track.flags, flag)
		}
	}

	// Helper to add flag and track new additions
	addFlag := func(name string) {
		if _, exists := track.flags[name]; !exists {
			flagsAddedThisSample++
		}
		track.flags[name] = now
	}

	// Check for invalid coordinates (NaN/Inf bypass range checks, must be explicit)
	if math.IsNaN(d.Latitude) || math.IsNaN(d.Longitude) ||
		math.IsInf(d.Latitude, 0) || math.IsInf(d.Longitude, 0) {
		addFlag("invalid_coordinates")
	} else if d.Latitude != 0 || d.Longitude != 0 {
		if d.Latitude < -90 || d.Latitude > 90 || d.Longitude < -180 || d.Longitude > 180 {
			addFlag("invalid_coordinates")
		}
	}

	// Check for missing serial
	if d.SerialNumber == "" {
		addFlag("no_serial")
	}

	// Check for randomized/locally administered MAC address
	// LAA bit is the second-least significant bit of the first octet
	if d.MACAddress != "" {
		if intel.IsLocallyAdministeredMAC(d.MACAddress) {
			addFlag("randomized_mac")
		}
	}

	// Check for OUI-SSID vendor mismatch (strong spoof indicator)
	// e.g., DJI OUI but SSID contains "ANAFI" or ESP32 claiming to be Matrice
	if d.MACAddress != "" && d.SSID != "" {
		if flag := checkOUISSIDConsistency(d.MACAddress, d.SSID); flag != "" {
			addFlag(flag)
		}
	}

	// Check for timestamp anomalies (detection time vs current time)
	// Flags drones with timestamps too far in future or past
	if !d.Timestamp.IsZero() {
		drift := now.Sub(d.Timestamp)
		// Flag if timestamp is >5s in future or >30s in past
		if drift < -5*time.Second || drift > time.Duration(maxTimestampDriftSec)*time.Second {
			addFlag("timestamp_anomaly")
		}
	}

	// RSSI-distance plausibility check
	// Strong RSSI (> -50 dBm) but drone claims to be far from any reasonable tap position
	// This is a simplified check - full implementation would need tap positions
	if d.RSSI > -40 && d.RSSI != 0 {
		// Very strong signal typically means <50m distance
		// If drone reports operator position and it's far, that's suspicious
		if d.OperatorLatitude != 0 && d.OperatorLongitude != 0 &&
			d.Latitude != 0 && d.Longitude != 0 {
			opDist := haversineKM(d.Latitude, d.Longitude, d.OperatorLatitude, d.OperatorLongitude) * 1000
			// If operator is >500m from drone but RSSI is extremely strong, suspicious
			if opDist > 500 {
				addFlag("rssi_distance_mismatch")
			}
		}
	}

	// rssi_impossible: RSSI value that defies physics
	// Check for physically impossible RSSI values or gross mismatches
	if d.RSSI != 0 {
		// RSSI stronger than -20 dBm is physically impossible for WiFi at any distance
		// (typical max TX power 20 dBm, antenna gains don't overcome path loss at 1m)
		if d.RSSI > -20 {
			addFlag("rssi_impossible")
		}
		// RSSI weaker than -100 dBm is below WiFi noise floor
		// (Realtek adapters report valid frames down to -95 dBm at long range)
		if d.RSSI < -100 {
			addFlag("rssi_impossible")
		}
		// Use RSSI distance estimation if we have model info
		// Strong signal (-40 dBm) typically max ~50m, weak (-80 dBm) typically max ~500m
		// If we have GPS showing drone is 2km+ away but RSSI is strong, flag it
		if d.RSSI > -50 && d.Latitude != 0 && d.Longitude != 0 {
			// Estimate distance from RSSI using calibration
			model := d.Model
			if model == "" {
				model = d.Designation
			}
			estDist := intel.EstimateDistanceFromRSSI(float64(d.RSSI), model, intel.DefaultPathLossN)
			// If estimated distance from RSSI is <100m but we somehow have position data
			// that would require being very far from any sensor, it's suspicious
			// This requires knowing tap positions - simplified check based on RSSI alone
			if estDist > 0 && estDist < 100 {
				// With RSSI suggesting <100m, drone altitude should also be low-to-moderate
				// A drone at 5000m AGL would have much weaker RSSI due to slant distance
				if d.HeightAGL > 2000 || d.AltitudeGeodetic > 5000 {
					addFlag("rssi_impossible")
				}
			}
		}
	}

	// If we have previous position, check for movement anomalies
	if exists && track.lastLat != 0 && d.Latitude != 0 {
		dt := now.Sub(track.lastTime).Seconds()
		if dt > 0.1 { // At least 100ms between updates
			// Calculate distance
			dist := haversineKM(track.lastLat, track.lastLng, d.Latitude, d.Longitude) * 1000

			// Check for impossible speed (coordinate jump)
			if dt > 0 {
				impliedSpeed := dist / dt
				if impliedSpeed > maxDroneSpeedMS && dist > 100 {
					addFlag("coordinate_jump")
				}
			}

			// Check altitude spike
			if track.lastAlt != 0 && d.AltitudeGeodetic != 0 {
				altChange := math.Abs(float64(d.AltitudeGeodetic - track.lastAlt))
				if altChange/dt > maxAltitudeChange {
					addFlag("altitude_spike")
				}
			}
		}
	}

	// Check speed violation
	if d.Speed > float32(maxDroneSpeedMS) {
		addFlag("speed_violation")
	}

	// Check detection confidence (from TAP's RemoteID parsing quality)
	// confidence >= 0.90: High quality RemoteID with location data
	// confidence >= 0.80: Protocol decoded successfully
	// confidence < 0.50: Low quality / partial decode
	if d.Confidence > 0 && d.Confidence < 0.50 {
		addFlag("low_confidence")
	}

	// Check for duplicate_id: same identifier from different MACs at different locations
	// This detects ID spoofing where multiple devices broadcast the same serial/identifier
	if d.Identifier != "" && d.MACAddress != "" && (d.Latitude != 0 || d.Longitude != 0) {
		if _, exists := shard.identifierMACs[d.Identifier]; !exists {
			shard.identifierMACs[d.Identifier] = make(map[string]*identifierMACInfo)
		}

		macMap := shard.identifierMACs[d.Identifier]

		// Check if we've seen this identifier from a DIFFERENT MAC recently
		for otherMAC, info := range macMap {
			if otherMAC == d.MACAddress {
				continue // Same MAC, skip
			}
			// Only consider recent observations (within 5 minutes)
			if now.Sub(info.lastSeen) > 5*time.Minute {
				continue
			}
			// Check if positions are significantly different (>100m apart)
			if info.lastLat != 0 && info.lastLng != 0 {
				dist := haversineKM(d.Latitude, d.Longitude, info.lastLat, info.lastLng) * 1000
				if dist > 100 {
					// Same identifier, different MAC, different location = likely spoofing
					addFlag("duplicate_id")
					break
				}
			}
		}

		// Update our tracking for this identifier-MAC pair
		macMap[d.MACAddress] = &identifierMACInfo{
			lastLat:  d.Latitude,
			lastLng:  d.Longitude,
			lastSeen: now,
		}
	}

	// Trust recovery: track consecutive clean samples
	if flagsAddedThisSample == 0 {
		track.consecutiveClean++
	} else {
		track.consecutiveClean = 0
	}

	// Update track
	track.lastLat = d.Latitude
	track.lastLng = d.Longitude
	track.lastAlt = d.AltitudeGeodetic
	track.lastSpeed = d.Speed
	track.lastRSSI = d.RSSI
	track.lastTime = now

	// Collect current flags
	flags := make([]string, 0, len(track.flags))
	for flag := range track.flags {
		flags = append(flags, flag)
	}

	// Calculate trust score
	totalPenalty := 0
	for _, flag := range flags {
		if p, ok := penalties[flag]; ok {
			totalPenalty += p
		}
	}

	trustScore := 100 - totalPenalty

	// Apply confidence bonus (high-quality RemoteID detections are more trustworthy)
	if d.Confidence >= 0.90 {
		// High confidence: RemoteID with full location data
		trustScore += bonuses["high_confidence"]
	} else if d.Confidence >= 0.80 {
		// Medium confidence: Protocol decoded successfully
		trustScore += bonuses["medium_confidence"]
	}

	// Apply dual/triple-signal correlation bonuses
	// These bonuses reward consistent identification across multiple sources
	correlationBonus := calculateCorrelationBonus(d)
	trustScore += correlationBonus

	// Trust recovery: if drone has been clean for a while, boost trust
	if track.consecutiveClean >= trustRecoveryClean && len(flags) == 0 {
		// Already clean, ensure full trust
		trustScore = 100
	} else if track.consecutiveClean >= trustRecoveryClean/2 && totalPenalty > 0 {
		// Partial recovery for sustained good behavior
		trustScore = trustScore + trustRecoveryAmount
	}

	// Clamp trust score
	if trustScore < 0 {
		trustScore = 0
	}
	if trustScore > 100 {
		trustScore = 100
	}

	return trustScore, flags
}

// checkOUISSIDConsistency detects when OUI vendor doesn't match SSID vendor
// This is a strong indicator of spoofing (e.g., DJI OUI but "ANAFI" in SSID)
// Now uses unified OUI database from intel package
func checkOUISSIDConsistency(mac, ssid string) string {
	if mac == "" || ssid == "" {
		return "" // Can't validate without both
	}

	mac = strings.ToUpper(strings.ReplaceAll(mac, "-", ":"))
	ssid = strings.ToUpper(ssid)

	// Get vendor from unified OUI database (intel.MatchOUI)
	ouiDesc := intel.MatchOUI(mac)
	if ouiDesc == "" {
		return "" // Unknown OUI, can't validate
	}

	// Extract vendor name from OUI description (e.g., "DJI (drone)" -> "DJI")
	ouiVendor := strings.ToUpper(extractVendorFromOUI(ouiDesc))

	// Check if SSID claims a specific vendor (uses package-level map)
	for vendor, patterns := range ssidVendorPatterns {
		for _, pattern := range patterns {
			if strings.Contains(ssid, pattern) {
				// Found a vendor claim in SSID
				// Check for ESP32 claiming to be a known brand
				if strings.Contains(ouiVendor, "ESP") || strings.Contains(ouiVendor, "ESPRESSIF") {
					if vendor == "DJI" || vendor == "PARROT" || vendor == "AUTEL" || vendor == "SKYDIO" {
						return "impossible_vendor"
					}
				}
				// Check for vendor mismatch (normalize both sides)
				if !vendorMatches(ouiVendor, vendor) {
					return "oui_ssid_mismatch"
				}
				return "" // Consistent
			}
		}
	}

	return "" // No mismatch detected
}

// extractVendorFromOUI extracts vendor name from OUI description
// e.g., "DJI (drone)" -> "DJI", "Parrot (controller)" -> "PARROT"
func extractVendorFromOUI(desc string) string {
	if desc == "" {
		return ""
	}
	// Remove parenthetical suffix like " (drone)" or " (controller)"
	if idx := strings.Index(desc, " ("); idx > 0 {
		return desc[:idx]
	}
	return desc
}

// vendorMatches checks if two vendor names refer to the same manufacturer
func vendorMatches(ouiVendor, ssidVendor string) bool {
	ouiVendor = strings.ToUpper(ouiVendor)
	ssidVendor = strings.ToUpper(ssidVendor)

	// Direct match
	if strings.Contains(ouiVendor, ssidVendor) || strings.Contains(ssidVendor, ouiVendor) {
		return true
	}

	// DJI variants: Goertek makes DJI Tello, DJI Baiwang
	if ssidVendor == "DJI" {
		if strings.Contains(ouiVendor, "GOERTEK") || strings.Contains(ouiVendor, "BAIWANG") {
			return true
		}
	}

	// Xiaomi/FIMI are related
	if ssidVendor == "FIMI" && strings.Contains(ouiVendor, "XIAOMI") {
		return true
	}

	return false
}

// calculateCorrelationBonus computes trust bonus for multi-signal correlation
// Higher bonuses for consistent identification across OUI, SSID, and RemoteID
func calculateCorrelationBonus(d *Drone) int {
	if d.MACAddress == "" {
		return 0 // Need MAC address for OUI matching
	}

	// Get OUI vendor
	ouiDesc := intel.MatchOUI(d.MACAddress)
	if ouiDesc == "" {
		return 0 // Unknown OUI, can't correlate
	}
	ouiVendor := strings.ToUpper(extractVendorFromOUI(ouiDesc))

	// Get SSID vendor (if available)
	ssidVendor := ""
	if d.SSID != "" {
		ssidVendor = extractVendorFromSSID(d.SSID)
	}

	// Get RemoteID manufacturer (if available)
	remoteIDVendor := ""
	hasRemoteID := false
	if d.Manufacturer != "" && d.DetectionSource != "wifi" {
		// Manufacturer field is typically set from RemoteID/DroneID
		remoteIDVendor = strings.ToUpper(d.Manufacturer)
		hasRemoteID = true
	}

	// Check for triple match: OUI + SSID + RemoteID + Serial
	if ssidVendor != "" && hasRemoteID && d.SerialNumber != "" {
		if vendorMatches(ouiVendor, ssidVendor) && vendorMatches(ouiVendor, remoteIDVendor) {
			return bonuses["triple_match_with_serial"] // +20
		}
	}

	// Check for triple match without serial: OUI + SSID + RemoteID
	if ssidVendor != "" && hasRemoteID {
		if vendorMatches(ouiVendor, ssidVendor) && vendorMatches(ouiVendor, remoteIDVendor) {
			return bonuses["oui_ssid_remoteid_match"] // +15
		}
	}

	// Check for dual match: OUI + SSID (no RemoteID)
	if ssidVendor != "" && !hasRemoteID {
		if vendorMatches(ouiVendor, ssidVendor) {
			return bonuses["oui_ssid_match"] // +10
		}
	}

	return 0
}

// extractVendorFromSSID extracts vendor name from SSID patterns
func extractVendorFromSSID(ssid string) string {
	ssid = strings.ToUpper(ssid)

	// Check each prefix — uses package-level vendorPrefixMap
	// Require word boundary for short patterns to avoid
	// false positives like "AIRPORT-WIFI" matching "AIR" as DJI
	for prefix, vendor := range vendorPrefixMap {
		if strings.HasPrefix(ssid, prefix) {
			return vendor
		}
		// For patterns >= 5 chars, substring match is safe enough
		// For shorter ones, require separator boundary (-, _, space)
		if len(prefix) >= 5 {
			if strings.Contains(ssid, prefix) {
				return vendor
			}
		} else {
			// Check for word-boundary match: preceded by separator or at start
			for _, sep := range []string{"-", "_", " "} {
				if strings.Contains(ssid, sep+prefix) {
					return vendor
				}
			}
		}
	}

	return ""
}

// haversineKM calculates great-circle distance in kilometers
func haversineKM(lat1, lng1, lat2, lng2 float64) float64 {
	lat1Rad := lat1 * math.Pi / 180
	lat2Rad := lat2 * math.Pi / 180
	dLat := (lat2 - lat1) * math.Pi / 180
	dLng := (lng2 - lng1) * math.Pi / 180

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1Rad)*math.Cos(lat2Rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return earthRadiusKM * c
}

// Cleanup removes old tracks and returns count of removed tracks
func (s *SpoofDetector) Cleanup(maxAge time.Duration) int {
	now := time.Now()
	removed := 0

	for i := 0; i < numSpoofShards; i++ {
		shard := s.shards[i]
		shard.mu.Lock()

		for id, track := range shard.tracks {
			if now.Sub(track.lastTime) > maxAge {
				delete(shard.tracks, id)
				removed++
			}
		}

		// Cleanup stale identifier-MAC mappings
		for identifier, macMap := range shard.identifierMACs {
			for mac, info := range macMap {
				if now.Sub(info.lastSeen) > maxAge {
					delete(macMap, mac)
				}
			}
			if len(macMap) == 0 {
				delete(shard.identifierMACs, identifier)
			}
		}

		shard.mu.Unlock()
	}

	return removed
}
