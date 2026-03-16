package processor

import (
	"context"
	"fmt"

	"log/slog"
	"sync"
	"time"
)

// Number of shards for drone map - power of 2 for fast modulo
const numDroneShards = 16

// droneShard holds a subset of drones with its own lock
type droneShard struct {
	mu     sync.RWMutex
	drones map[string]*Drone
}

// shardedDrones distributes drones across multiple shards to reduce lock contention
type shardedDrones struct {
	shards [numDroneShards]*droneShard

	// Secondary indexes for O(1) lookup by serial/MAC (avoid O(N) scan across all shards)
	idxMu    sync.RWMutex
	bySerial map[string]string // serial_number -> identifier
	byMAC    map[string]string // mac_address -> identifier
}

// newShardedDrones creates a new sharded drone map
func newShardedDrones() *shardedDrones {
	sd := &shardedDrones{
		bySerial: make(map[string]string),
		byMAC:    make(map[string]string),
	}
	for i := 0; i < numDroneShards; i++ {
		sd.shards[i] = &droneShard{
			drones: make(map[string]*Drone),
		}
	}
	return sd
}

// getShard returns the shard for a given identifier using inline FNV-1a (zero allocation)
func (sd *shardedDrones) getShard(identifier string) *droneShard {
	var h uint32 = 2166136261 // FNV-1a offset basis
	for i := 0; i < len(identifier); i++ {
		h ^= uint32(identifier[i])
		h *= 16777619 // FNV-1a prime
	}
	return sd.shards[h&(numDroneShards-1)] // Bitwise AND (numDroneShards is power of 2)
}

// get retrieves a drone by identifier
func (sd *shardedDrones) get(identifier string) (*Drone, bool) {
	shard := sd.getShard(identifier)
	shard.mu.RLock()
	d, ok := shard.drones[identifier]
	shard.mu.RUnlock()
	return d, ok
}

// set stores a drone and updates secondary indexes
func (sd *shardedDrones) set(identifier string, d *Drone) {
	shard := sd.getShard(identifier)
	shard.mu.Lock()
	shard.drones[identifier] = d
	shard.mu.Unlock()
	sd.updateIndexes(identifier, d.SerialNumber, d.MACAddress)
}

// delete removes a drone and cleans secondary indexes
func (sd *shardedDrones) delete(identifier string) bool {
	shard := sd.getShard(identifier)
	shard.mu.Lock()
	d, exists := shard.drones[identifier]
	if exists {
		delete(shard.drones, identifier)
	}
	shard.mu.Unlock()
	if exists && d != nil {
		sd.removeFromIndexes(identifier, d.SerialNumber, d.MACAddress)
	}
	return exists
}

// updateIndexes adds or updates secondary index entries for a drone
func (sd *shardedDrones) updateIndexes(identifier, serial, mac string) {
	sd.idxMu.Lock()
	if serial != "" {
		sd.bySerial[serial] = identifier
	}
	if mac != "" {
		sd.byMAC[mac] = identifier
	}
	sd.idxMu.Unlock()
}

// removeFromIndexes removes secondary index entries for a drone
func (sd *shardedDrones) removeFromIndexes(identifier, serial, mac string) {
	sd.idxMu.Lock()
	if serial != "" {
		if sd.bySerial[serial] == identifier {
			delete(sd.bySerial, serial)
		}
	}
	if mac != "" {
		if sd.byMAC[mac] == identifier {
			delete(sd.byMAC, mac)
		}
	}
	sd.idxMu.Unlock()
}

// lookupBySerial returns the identifier of a drone with the given serial, or "" if not found
func (sd *shardedDrones) lookupBySerial(serial string) string {
	sd.idxMu.RLock()
	id := sd.bySerial[serial]
	sd.idxMu.RUnlock()
	return id
}

// lookupByMAC returns the identifier of a drone with the given MAC, or "" if not found
func (sd *shardedDrones) lookupByMAC(mac string) string {
	sd.idxMu.RLock()
	id := sd.byMAC[mac]
	sd.idxMu.RUnlock()
	return id
}

// getAll returns all drones (locks each shard sequentially)
func (sd *shardedDrones) getAll() []*Drone {
	result := make([]*Drone, 0, sd.count())
	for i := 0; i < numDroneShards; i++ {
		shard := sd.shards[i]
		shard.mu.RLock()
		for _, d := range shard.drones {
			result = append(result, d)
		}
		shard.mu.RUnlock()
	}
	return result
}

// count returns total number of drones
func (sd *shardedDrones) count() int {
	total := 0
	for i := 0; i < numDroneShards; i++ {
		shard := sd.shards[i]
		shard.mu.RLock()
		total += len(shard.drones)
		shard.mu.RUnlock()
	}
	return total
}

// forEach iterates over all drones with a callback (acquires read lock per shard)
func (sd *shardedDrones) forEach(fn func(*Drone)) {
	for i := 0; i < numDroneShards; i++ {
		shard := sd.shards[i]
		shard.mu.RLock()
		for _, d := range shard.drones {
			fn(d)
		}
		shard.mu.RUnlock()
	}
}

// forEachMut iterates over all drones with mutation allowed (acquires write lock per shard)
func (sd *shardedDrones) forEachMut(fn func(*Drone)) {
	for i := 0; i < numDroneShards; i++ {
		shard := sd.shards[i]
		shard.mu.Lock()
		for _, d := range shard.drones {
			fn(d)
		}
		shard.mu.Unlock()
	}
}

// getByMAC finds a drone by MAC address using the secondary index (O(1))
func (sd *shardedDrones) getByMAC(mac string) *Drone {
	id := sd.lookupByMAC(mac)
	if id == "" {
		return nil
	}
	d, _ := sd.get(id)
	return d
}

// RangeRing represents a distance measurement from a single tap
// Used for visualization of RSSI-based distance estimation
type RangeRing struct {
	TapID      string  `json:"tap_id"`              // Source tap identifier
	TapLat     float64 `json:"tap_lat,omitempty"`   // Tap latitude
	TapLon     float64 `json:"tap_lon,omitempty"`   // Tap longitude
	DistanceM  float64 `json:"distance_m"`          // Best estimate distance in meters
	MinM       float64 `json:"min_m"`               // Lower bound (1-sigma)
	MaxM       float64 `json:"max_m"`               // Upper bound (1-sigma)
	Confidence float64 `json:"confidence"`          // Measurement confidence 0-1
	RSSI       int32   `json:"rssi,omitempty"`      // Original RSSI value
	Environment string    `json:"environment,omitempty"` // Environment type used
	UpdatedAt   time.Time `json:"-"`                     // When this ring was last updated (for cleanup)
}

// EstimatedPosition holds trilaterated position from multiple taps
type EstimatedPosition struct {
	Latitude   float64 `json:"latitude"`            // Estimated latitude
	Longitude  float64 `json:"longitude"`           // Estimated longitude
	ErrorM     float64 `json:"error_m"`             // Position uncertainty radius
	Confidence float64 `json:"confidence"`          // Overall confidence 0-1
	TapsUsed   int     `json:"taps_used"`           // Number of taps used
	Method     string  `json:"method"`              // Algorithm used
	SemiMajorM float64 `json:"semi_major_m"`        // Error ellipse semi-major axis
	SemiMinorM float64 `json:"semi_minor_m"`        // Error ellipse semi-minor axis
	Timestamp  time.Time `json:"timestamp"`         // When position was computed
}

// Drone represents the current state of a detected drone
type Drone struct {
	// Identification
	Identifier   string `json:"identifier"`
	MACAddress   string `json:"mac,omitempty"` // dashboard uses "mac"
	SerialNumber string `json:"serial_number,omitempty"`
	Registration string `json:"registration,omitempty"`
	SessionID    string `json:"session_id,omitempty"`    // RemoteID session ID
	UTMID        string `json:"utm_id,omitempty"`        // UTM assigned ID
	Designation  string `json:"designation,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Model        string `json:"model,omitempty"`
	SSID         string `json:"ssid,omitempty"`

	// Position — no omitempty so 0-values propagate to frontend when GPS is cleared
	Latitude         float64 `json:"latitude"`
	Longitude        float64 `json:"longitude"`
	AltitudeGeodetic float32 `json:"altitude_geodetic,omitempty"`
	AltitudePressure float32 `json:"altitude_pressure,omitempty"`
	HeightAGL        float32 `json:"height_agl,omitempty"`
	HeightReference  string  `json:"height_reference,omitempty"` // TAKEOFF, GROUND, UNKNOWN

	// Movement — no omitempty on speed so 0 propagates when dark
	Speed         float32 `json:"speed"`
	VerticalSpeed float32 `json:"vertical_speed"`
	Heading       float32 `json:"heading,omitempty"`
	GroundTrack   float32 `json:"ground_track,omitempty"` // dashboard uses ground_track for velocity vector

	// Operator — no omitempty so 0-values propagate when operator position is cleared
	OperatorLatitude     float64 `json:"operator_latitude"`
	OperatorLongitude    float64 `json:"operator_longitude"`
	OperatorAltitude     float32 `json:"operator_altitude,omitempty"`
	OperatorID           string  `json:"operator_id,omitempty"`
	OperatorLocationType string  `json:"operator_location_type,omitempty"` // TAKEOFF, LIVE_GNSS, FIXED

	// Signal
	RSSI             int32  `json:"rssi,omitempty"`
	Channel          int32  `json:"channel,omitempty"`
	FrequencyMHz     int32  `json:"frequency_mhz,omitempty"`      // Exact frequency if known
	BeaconIntervalTU uint32 `json:"beacon_interval_tu,omitempty"` // Beacon interval in Time Units (typical drone: 100)
	CountryCode      string `json:"country_code,omitempty"`       // 802.11d country code (e.g. "US", "CN")
	HTCapabilities   uint32 `json:"ht_capabilities,omitempty"`    // HT Capabilities info (device fingerprint)

	// Detection
	DetectionSource   string  `json:"detection_source"` // dashboard expects detection_source
	UAVType           string  `json:"uav_type,omitempty"`
	UAVCategory       string  `json:"uav_category,omitempty"`
	OperationalStatus string  `json:"operational_status,omitempty"` // GROUND, AIRBORNE, EMERGENCY
	Confidence        float32 `json:"confidence,omitempty"`         // Detection confidence 0.0-1.0
	IsController      bool    `json:"is_controller,omitempty"`      // True if this is a controller, not a drone

	// Trust & Classification
	TrustScore     int      `json:"trust_score"`
	SpoofFlags     []string `json:"spoof_flags,omitempty"`
	Classification string   `json:"classification"`
	Tag            string   `json:"tag,omitempty"`

	// Status
	Status         string    `json:"status"`           // "active" or "lost"
	ContactStatus  string    `json:"_contactStatus"`   // dashboard uses _contactStatus
	FirstSeen      time.Time `json:"first_seen"`
	LastSeen       time.Time `json:"last_seen"`
	Timestamp      time.Time `json:"timestamp"`        // dashboard also uses timestamp
	DetectionCount int64     `json:"detection_count"`
	TrackNumber    int       `json:"track_number,omitempty"`
	Hidden         bool      `json:"hidden,omitempty"` // for hide/unhide functionality

	// Source tap
	TapID string `json:"tap_id"`

	// Internal: last time a detection included valid GPS (not serialized)
	LastGPSTime time.Time `json:"-"`

	// Distance estimate (best single-tap estimate in meters)
	DistanceEstM float64 `json:"distance_est_m,omitempty"`

	// Range Rings - RSSI-based distance estimates from each detecting tap
	RangeRings []RangeRing `json:"range_rings,omitempty"`

	// Estimated Position - trilaterated position from multiple taps
	EstimatedPos *EstimatedPosition `json:"estimated_position,omitempty"`

	// Controller-UAV linking
	LinkedControllerID string `json:"linked_controller_id,omitempty"` // If this is a UAV, the linked controller identifier
	LinkedUAVID        string `json:"linked_uav_id,omitempty"`        // If this is a controller, the linked UAV identifier

	// Raw data (optional, for analysis)
	RawFrame        []byte `json:"raw_frame,omitempty"`
	RemoteIDPayload []byte `json:"remoteid_payload,omitempty"`
}

// Tap represents a connected tap sensor
type Tap struct {
	ID        string    `json:"tap_uuid"` // dashboard uses tap_uuid
	Name      string    `json:"tap_name"` // dashboard uses tap_name
	Latitude  float64   `json:"latitude"`
	Longitude float64   `json:"longitude"`
	Altitude  float32   `json:"altitude"`
	Status    string    `json:"status"`    // "online" or "offline"
	LastSeen  time.Time `json:"timestamp"` // dashboard uses timestamp
	Version   string    `json:"version"`

	// Stats - dashboard field names
	FramesTotal        uint64  `json:"frames_total"`          // dashboard uses frames_total
	PacketsCaptured    uint64  `json:"packets_captured"`      // alias
	PacketsFiltered    uint64  `json:"packets_filtered"`
	DetectionsSent     uint64  `json:"detections_sent"`
	CurrentChannel     int32   `json:"current_channel"`
	PacketsPerSecond   float32 `json:"packets_per_second"`
	PcapKernelReceived uint64  `json:"pcap_kernel_received"`  // Kernel buffer received
	PcapKernelDropped  uint64  `json:"pcap_kernel_dropped"`   // Kernel buffer dropped (warning if > 0)
	CPUPercent         float32 `json:"cpu_percent"`
	MemoryPercent      float32 `json:"memory_percent"`
	Temperature        float32 `json:"temperature"`
	CaptureRunning     bool    `json:"capture_running"` // TAP capture status
	TapUptime          int64   `json:"tap_uptime"`      // seconds

	// Buffer and NATS health (from TAP heartbeat)
	BufferSize       uint64  `json:"buffer_size"`
	BufferDropped    uint64  `json:"buffer_dropped"`
	PublishRetries   uint64  `json:"publish_retries"`
	NATSDisconnects  uint64  `json:"nats_disconnects"`
	NATSReconnects   uint64  `json:"nats_reconnects"`
	TotalBSSIDs      uint64  `json:"total_bssids"`
	DistinctChannels uint32  `json:"distinct_channels"`
	WiFiInterface    string  `json:"wifi_interface"`

	// BLE stats
	BLEInterface      string `json:"ble_interface,omitempty"`
	BLEAdvertisements uint64 `json:"ble_advertisements"`
	BLEDetections     uint64 `json:"ble_detections"`
	BLEScanning       bool    `json:"ble_scanning"`
	SeenChannels      []int32 `json:"seen_channels"` // Channels observed from heartbeats
}

// SuspectCandidate represents a potential drone detection that has not yet been
// confirmed through multi-TAP correlation. These are WiFi signals that match
// drone signatures but lack RemoteID or other positive identification.
type SuspectCandidate struct {
	Identifier      string               `json:"identifier"`       // Primary key (usually MAC-based)
	MACAddress      string               `json:"mac_address"`      // WiFi MAC address
	FirstSeen       time.Time            `json:"first_seen"`       // When first detected
	LastSeen        time.Time            `json:"last_seen"`        // Most recent detection
	TapsSeen        map[string]time.Time `json:"taps_seen"`        // TAP ID -> last seen time
	Observations    int                  `json:"observations"`     // Total detection count
	BestConfidence  float32              `json:"best_confidence"`  // Highest confidence seen
	Channel         int32                `json:"channel"`          // WiFi channel
	AvgRSSI         float64              `json:"avg_rssi"`         // Running average RSSI
	BehavioralFlags []string             `json:"behavioral_flags"` // Flags from behavior analysis
	MobilityScore   float64              `json:"mobility_score"`   // 0-1 movement indicator
	RemoteIDUpgrade bool                 `json:"remoteid_upgrade"` // True if later matched to RemoteID

	// Single-TAP mode tracking fields
	RSSIVariance     float64 `json:"rssi_variance"`      // Variance in RSSI (high = likely mobile)
	RSSIMin          int32   `json:"rssi_min"`           // Minimum RSSI observed
	RSSIMax          int32   `json:"rssi_max"`           // Maximum RSSI observed
	ChannelChanges   int     `json:"channel_changes"`    // Number of channel changes observed
	IsLikelyMobile   bool    `json:"is_likely_mobile"`   // Computed: likely a mobile drone vs static interference
	ManuallyConfirmed bool   `json:"manually_confirmed"` // Operator confirmed via API
}

// StateManager maintains the current state of all drones and taps
type StateManager struct {
	// Sharded drone storage for reduced lock contention
	drones *shardedDrones

	// Taps have separate lock (low frequency updates)
	tapMu sync.RWMutex
	taps  map[string]*Tap
	cfg   DetectionConfig

	// Suspect candidate tracking
	suspectMu         sync.RWMutex
	suspectCandidates map[string]*SuspectCandidate
	correlator        *SuspectCorrelator

	// Controller-UAV linking
	controllerMu    sync.RWMutex
	controllerLinks map[string]*ControllerLink // key = controller identifier

	// GPS trail management for real-time visualization
	trails *TrailManager

	// Track number counter (monotonic, persisted via DB)
	trackMu      sync.Mutex
	nextTrackNum int

	// Subscribers for real-time updates
	subMu       sync.RWMutex
	subscribers []chan<- StateEvent
}

type DetectionConfig struct {
	LostThresholdSec   int
	EvictAfterMin      int // Minutes after last seen to evict lost drones from memory (default: 30)
	TrustDecayRate     float64
	MaxHistoryHours    int
	MaxDisplayedDrones int
	SpoofCheckEnabled  bool

	// SingleTapMode enables promotion of suspects with only one TAP.
	// When true, suspects can be auto-promoted based on:
	// - High mobility score (RSSI variance suggesting movement)
	// - Multiple observations over time (>=5 obs, >=30s timespan)
	// Multi-TAP correlation remains a confidence booster, not a gate.
	SingleTapMode bool

	// SingleTapPromotion thresholds (only used when SingleTapMode=true)
	SingleTapMinObservations int     // Minimum observations for single-TAP promotion (default: 5)
	SingleTapMinTimeSpanSec  int     // Minimum time span in seconds (default: 30)
	SingleTapMinMobility     float64 // Minimum mobility score for auto-promotion (default: 0.6)
}

// StateEvent is sent to subscribers on state changes
type StateEvent struct {
	Type string      `json:"type"` // "drone_new", "drone_update", "drone_lost", "tap_status"
	Data interface{} `json:"data"`
}

// HydrateDrone adds a drone to the state without triggering broadcasts.
// Used during startup to restore state from database.
func (s *StateManager) HydrateDrone(d *Drone) {
	shard := s.drones.getShard(d.Identifier)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	// Only add if not already present (shouldn't happen, but be safe)
	if _, exists := shard.drones[d.Identifier]; !exists {
		shard.drones[d.Identifier] = d
	}
}

// HydrateDrones bulk-loads drones into state without triggering broadcasts.
// Used during startup to restore state from database.
// Returns the number of drones hydrated.
func (s *StateManager) HydrateDrones(drones []*Drone) int {
	count := 0
	for _, d := range drones {
		shard := s.drones.getShard(d.Identifier)
		shard.mu.Lock()
		if _, exists := shard.drones[d.Identifier]; !exists {
			shard.drones[d.Identifier] = d
			count++
			s.drones.updateIndexes(d.Identifier, d.SerialNumber, d.MACAddress)
		}
		shard.mu.Unlock()
	}
	return count
}

// NewStateManager creates a new state manager
func NewStateManager(cfg DetectionConfig) *StateManager {
	// Apply defaults for single-TAP mode thresholds
	if cfg.SingleTapMinObservations <= 0 {
		cfg.SingleTapMinObservations = 5
	}
	if cfg.SingleTapMinTimeSpanSec <= 0 {
		cfg.SingleTapMinTimeSpanSec = 30
	}
	if cfg.SingleTapMinMobility <= 0 {
		cfg.SingleTapMinMobility = 0.6
	}
	// EvictAfterMin: 0 = disabled (keep lost drones in memory forever)
	// Only set a default if not explicitly configured

	sm := &StateManager{
		drones:            newShardedDrones(),
		taps:              make(map[string]*Tap),
		cfg:               cfg,
		subscribers:       make([]chan<- StateEvent, 0),
		suspectCandidates: make(map[string]*SuspectCandidate),
		controllerLinks:   make(map[string]*ControllerLink),
		trails:            NewTrailManager(DefaultTrailConfig()),
	}
	// Initialize the suspect correlator with:
	// - 30 second correlation window
	// - 2 TAPs minimum for multi-TAP confirmation (high confidence)
	// - 0.30 confidence boost per additional TAP
	// - SingleTapMode flag for single-TAP deployments
	sm.correlator = NewSuspectCorrelator(30, 2, 0.30, cfg.SingleTapMode)
	return sm
}

// SetNextTrackNum restores the track counter from the database on startup.
func (s *StateManager) SetNextTrackNum(n int) {
	s.trackMu.Lock()
	s.nextTrackNum = n
	s.trackMu.Unlock()
}

// assignTrackNumber assigns the next sequential track number to a drone.
func (s *StateManager) assignTrackNumber(d *Drone) {
	s.trackMu.Lock()
	s.nextTrackNum++
	d.TrackNumber = s.nextTrackNum
	s.trackMu.Unlock()
}

// TrackNumberSaver persists track numbers to the database.
type TrackNumberSaver interface {
	UpdateTrackNumber(identifier string, trackNumber int) error
}

// BackfillTrackNumbers assigns track numbers to all existing drones that don't have one.
// Controllers are excluded — only real UAVs get track numbers.
// If a saver is provided, track numbers are persisted to the database.
func (s *StateManager) BackfillTrackNumbers(saver TrackNumberSaver) int {
	all := s.drones.getAll()
	count := 0
	for _, d := range all {
		if d.TrackNumber == 0 && !d.IsController {
			s.assignTrackNumber(d)
			if saver != nil {
				saver.UpdateTrackNumber(d.Identifier, d.TrackNumber)
			}
			count++
		}
	}
	return count
}

// Subscribe adds a subscriber for state events
func (s *StateManager) Subscribe(ch chan<- StateEvent) {
	s.subMu.Lock()
	s.subscribers = append(s.subscribers, ch)
	s.subMu.Unlock()
}

// Unsubscribe removes a subscriber
func (s *StateManager) Unsubscribe(ch chan<- StateEvent) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for i, sub := range s.subscribers {
		if sub == ch {
			s.subscribers = append(s.subscribers[:i], s.subscribers[i+1:]...)
			return
		}
	}
}

// broadcast sends an event to all subscribers
func (s *StateManager) broadcast(event StateEvent) {
	s.subMu.RLock()
	defer s.subMu.RUnlock()
	for _, ch := range s.subscribers {
		select {
		case ch <- event:
		default:
			// Subscriber is slow, skip
		}
	}
}

// BroadcastRefresh sends a system_refresh event to all WebSocket clients,
// signaling them to re-fetch state. Used after NATS reconnection or system resume.
func (s *StateManager) BroadcastRefresh() {
	s.broadcast(StateEvent{Type: "system_refresh", Data: map[string]string{"reason": "reconnect"}})
}

// BroadcastDroneUpdate sends a drone_update event for the given drone ID.
// Used after modifying drone data (e.g., range rings, estimated position)
// outside of UpdateDrone to ensure WebSocket clients see the changes.
func (s *StateManager) BroadcastDroneUpdate(droneID string) {
	d, ok := s.drones.get(droneID)
	if !ok {
		return
	}
	s.broadcast(StateEvent{Type: "drone_update", Data: d})
}

// UpdateDrone updates or creates a drone in the state.
// Returns (isNew, resolvedID) where resolvedID is the actual identifier
// the drone is stored under (may differ from d.Identifier if merged by serial/MAC).
func (s *StateManager) UpdateDrone(d *Drone) (isNew bool, resolvedID string) {
	if d.Identifier == "" {
		slog.Warn("Rejecting drone with empty identifier", "mac", d.MACAddress, "serial", d.SerialNumber)
		return false, ""
	}

	// First check if drone with same serial exists under different identifier
	// This prevents duplicates when same drone is detected via different sources
	if d.SerialNumber != "" {
		if existingID := s.findDroneBySerial(d.SerialNumber, d.Identifier); existingID != "" {
			// Update the existing drone instead
			existingShard := s.drones.getShard(existingID)
			existingShard.mu.Lock()
			if existing, ok := existingShard.drones[existingID]; ok {
				s.mergeIntoExisting(existing, d)
				existingShard.mu.Unlock()
				s.broadcast(StateEvent{Type: "drone_update", Data: existing})
				return false, existingID
			}
			existingShard.mu.Unlock()
		}
	}

	// Also check by MAC address if serial wasn't found
	if d.MACAddress != "" {
		if existingID := s.findDroneByMAC(d.MACAddress, d.Identifier); existingID != "" {
			existingShard := s.drones.getShard(existingID)
			existingShard.mu.Lock()
			if existing, ok := existingShard.drones[existingID]; ok {
				s.mergeIntoExisting(existing, d)
				existingShard.mu.Unlock()
				s.broadcast(StateEvent{Type: "drone_update", Data: existing})
				return false, existingID
			}
			existingShard.mu.Unlock()
		}
	}

	shard := s.drones.getShard(d.Identifier)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	existing, exists := shard.drones[d.Identifier]
	now := time.Now()

	if !exists {
		// New drone
		d.FirstSeen = now
		d.LastSeen = now
		d.Timestamp = now
		d.Status = "active"
		d.ContactStatus = "active"
		d.DetectionCount = 1
		if !d.IsController {
			s.assignTrackNumber(d)
		}
		if d.TrustScore == 0 {
			d.TrustScore = 100
		}
		if d.Classification == "" {
			d.Classification = "UNKNOWN"
		}
		// Set ground_track from heading if not set
		if d.GroundTrack == 0 && d.Heading != 0 {
			d.GroundTrack = d.Heading
		}
		shard.drones[d.Identifier] = d
		// Update secondary indexes for new drone
		s.drones.updateIndexes(d.Identifier, d.SerialNumber, d.MACAddress)

		// Record initial position in trail if we have valid GPS
		if d.Latitude != 0 && d.Longitude != 0 && s.trails != nil {
			s.trails.RecordPosition(d.Identifier, PositionSample{
				Time:    now,
				Lat:     d.Latitude,
				Lon:     d.Longitude,
				Alt:     d.AltitudeGeodetic,
				AltAGL:  d.HeightAGL,
				Speed:   d.Speed,
				Heading: d.GroundTrack,
				VSpeed:  d.VerticalSpeed,
				RSSI:    d.RSSI,
				TapID:   d.TapID,
			})
		}

		s.broadcast(StateEvent{Type: "drone_new", Data: d})
		slog.Info("New drone detected",
			"identifier", d.Identifier,
			"designation", d.Designation,
			"source", d.DetectionSource,
		)
		return true, d.Identifier
	}

	// Check if drone was lost BEFORE setting it active (for GPS stale timer logic)
	wasLost := existing.Status == "lost" || existing.ContactStatus == "lost"

	// Update existing drone
	existing.LastSeen = now
	existing.Timestamp = now
	existing.DetectionCount++
	existing.Status = "active"
	existing.ContactStatus = "active"

	// Update fields if new data is present.
	// WiFi-only = beacon detection with no GPS (lat/lon=0, RSSI present).
	// Only clear stale GPS after 60s of no GPS updates — a beacon arriving
	// between NAN frames must NOT wipe the GPS from the preceding NAN.
	isWifiOnly := d.Latitude == 0 && d.Longitude == 0 && d.RSSI != 0
	if d.Latitude != 0 {
		existing.Latitude = d.Latitude
		existing.LastGPSTime = time.Now()
	} else if isWifiOnly && existing.Latitude != 0 {
		if wasLost {
			// Drone just came back — give GPS detections 60s to arrive
			existing.LastGPSTime = time.Now()
		} else if !existing.LastGPSTime.IsZero() && time.Since(existing.LastGPSTime) > 60*time.Second {
			// Actively tracked drone stopped sending GPS for >60s
			existing.Latitude = 0
			existing.Longitude = 0
		}
	}
	if d.Longitude != 0 {
		existing.Longitude = d.Longitude
	}
	if d.AltitudeGeodetic != 0 {
		existing.AltitudeGeodetic = d.AltitudeGeodetic
	}
	if d.Speed != 0 {
		existing.Speed = d.Speed
	} else if isWifiOnly && !existing.LastGPSTime.IsZero() && time.Since(existing.LastGPSTime) > 60*time.Second {
		existing.Speed = 0
	}
	if d.VerticalSpeed != 0 {
		existing.VerticalSpeed = d.VerticalSpeed
	} else if isWifiOnly && !existing.LastGPSTime.IsZero() && time.Since(existing.LastGPSTime) > 60*time.Second {
		existing.VerticalSpeed = 0
	}
	if d.Heading != 0 {
		existing.Heading = d.Heading
		existing.GroundTrack = d.Heading // sync ground_track with heading
	}
	if d.GroundTrack != 0 {
		existing.GroundTrack = d.GroundTrack
	}
	if d.RSSI != 0 {
		existing.RSSI = d.RSSI
	}
	if d.Channel != 0 {
		existing.Channel = d.Channel
	}
	// Update operator position:
	// - Reject if within 10m of drone (bogus home/takeoff echo)
	// - Reject if >50km from drone (garbage/misparse from ODID framing)
	if d.OperatorLatitude != 0 && d.OperatorLongitude != 0 {
		rejectOp := false
		if existing.Latitude != 0 && existing.Longitude != 0 {
			dLat := (d.OperatorLatitude - existing.Latitude) * 111320
			dLon := (d.OperatorLongitude - existing.Longitude) * 111320 * 0.946 // cos(18°)
			distSq := dLat*dLat + dLon*dLon
			rejectOp = distSq < 100 || distSq > 50000*50000 // <10m or >50km
		}
		if !rejectOp {
			existing.OperatorLatitude = d.OperatorLatitude
			existing.OperatorLongitude = d.OperatorLongitude
		}
	} else if isWifiOnly && !existing.LastGPSTime.IsZero() && time.Since(existing.LastGPSTime) > 60*time.Second {
		existing.OperatorLatitude = 0
		existing.OperatorLongitude = 0
	}
	if d.SerialNumber != "" {
		existing.SerialNumber = d.SerialNumber
	}
	// Only update designation if the new one is better than existing
	// Don't overwrite good designations with generic ones
	genericDesignations := map[string]bool{
		"":                            true,
		"UNKNOWN":                     true,
		"Unknown":                     true,
		"WiFi RemoteID broadcast":     true,
		"DJI WiFi RemoteID broadcast": true,
		"RemoteID":                    true,
	}
	if d.Designation != "" && !genericDesignations[d.Designation] {
		// New designation is specific, use it
		existing.Designation = d.Designation
	} else if existing.Designation == "" || genericDesignations[existing.Designation] {
		// Existing is empty/generic, use new even if generic
		if d.Designation != "" {
			existing.Designation = d.Designation
		}
	}
	if d.Model != "" {
		existing.Model = d.Model
	}
	if d.TapID != "" {
		existing.TapID = d.TapID
	}
	if d.TrustScore != 0 {
		existing.TrustScore = d.TrustScore
	}
	if len(d.SpoofFlags) > 0 {
		existing.SpoofFlags = d.SpoofFlags
	}
	if d.DetectionSource != "" {
		existing.DetectionSource = d.DetectionSource
	}

	// Refresh secondary indexes in case serial/MAC was added to existing drone
	s.drones.updateIndexes(existing.Identifier, existing.SerialNumber, existing.MACAddress)

	// Record position in trail if we have valid GPS coordinates
	if existing.Latitude != 0 && existing.Longitude != 0 && s.trails != nil {
		s.trails.RecordPosition(existing.Identifier, PositionSample{
			Time:    now,
			Lat:     existing.Latitude,
			Lon:     existing.Longitude,
			Alt:     existing.AltitudeGeodetic,
			AltAGL:  existing.HeightAGL,
			Speed:   existing.Speed,
			Heading: existing.GroundTrack,
			VSpeed:  existing.VerticalSpeed,
			RSSI:    existing.RSSI,
			TapID:   existing.TapID,
		})
	}

	s.broadcast(StateEvent{Type: "drone_update", Data: existing})
	return false, existing.Identifier
}

// findDroneBySerial searches for a drone with the given serial number
// excludeID is the identifier to skip (the incoming drone's ID)
func (s *StateManager) findDroneBySerial(serial, excludeID string) string {
	id := s.drones.lookupBySerial(serial)
	if id != "" && id != excludeID {
		return id
	}
	return ""
}

// findDroneByMAC searches for a drone with the given MAC address
// excludeID is the identifier to skip (the incoming drone's ID)
func (s *StateManager) findDroneByMAC(mac, excludeID string) string {
	id := s.drones.lookupByMAC(mac)
	if id != "" && id != excludeID {
		return id
	}
	return ""
}

// mergeIntoExisting merges new drone data into an existing drone
func (s *StateManager) mergeIntoExisting(existing, d *Drone) {
	now := time.Now()
	wasLost := existing.Status == "lost" || existing.ContactStatus == "lost"
	existing.LastSeen = now
	existing.Timestamp = now
	existing.Status = "active"
	existing.ContactStatus = "active"
	existing.DetectionCount++

	// Detect WiFi-only detection (no GPS, has RSSI)
	isWifiOnly := d.Latitude == 0 && d.Longitude == 0 && d.RSSI != 0

	// Update fields with new data (preserve existing if new is empty/zero)
	if d.MACAddress != "" && existing.MACAddress == "" {
		existing.MACAddress = d.MACAddress
	}
	if d.Latitude != 0 {
		existing.Latitude = d.Latitude
		existing.LastGPSTime = time.Now()
	} else if isWifiOnly && existing.Latitude != 0 {
		if wasLost {
			// Drone just came back — give GPS detections 60s to arrive
			existing.LastGPSTime = time.Now()
		} else if !existing.LastGPSTime.IsZero() && time.Since(existing.LastGPSTime) > 60*time.Second {
			// Actively tracked drone stopped sending GPS for >60s
			existing.Latitude = 0
			existing.Longitude = 0
		}
	}
	if d.Longitude != 0 {
		existing.Longitude = d.Longitude
	}
	if d.AltitudeGeodetic != 0 {
		existing.AltitudeGeodetic = d.AltitudeGeodetic
	}
	if d.Speed != 0 {
		existing.Speed = d.Speed
	} else if isWifiOnly && !existing.LastGPSTime.IsZero() && time.Since(existing.LastGPSTime) > 60*time.Second {
		existing.Speed = 0
	}
	if d.Heading != 0 {
		existing.Heading = d.Heading
		existing.GroundTrack = d.Heading
	}
	if d.GroundTrack != 0 {
		existing.GroundTrack = d.GroundTrack
	}
	if d.VerticalSpeed != 0 {
		existing.VerticalSpeed = d.VerticalSpeed
	} else if isWifiOnly && !existing.LastGPSTime.IsZero() && time.Since(existing.LastGPSTime) > 60*time.Second {
		existing.VerticalSpeed = 0
	}
	if d.HeightAGL != 0 {
		existing.HeightAGL = d.HeightAGL
	}
	if d.RSSI != 0 {
		existing.RSSI = d.RSSI
	}
	if d.Channel != 0 {
		existing.Channel = d.Channel
	}
	if d.TapID != "" {
		existing.TapID = d.TapID
	}
	if d.Designation != "" && existing.Designation == "" {
		existing.Designation = d.Designation
	}
	if d.Model != "" && existing.Model == "" {
		existing.Model = d.Model
	}
	if d.OperatorLatitude != 0 && d.OperatorLongitude != 0 {
		rejectOp := false
		if existing.Latitude != 0 && existing.Longitude != 0 {
			dLat := (d.OperatorLatitude - existing.Latitude) * 111320
			dLon := (d.OperatorLongitude - existing.Longitude) * 111320 * 0.946
			distSq := dLat*dLat + dLon*dLon
			rejectOp = distSq < 100 || distSq > 50000*50000 // <10m or >50km
		}
		if !rejectOp {
			existing.OperatorLatitude = d.OperatorLatitude
			existing.OperatorLongitude = d.OperatorLongitude
		}
	} else if isWifiOnly && !existing.LastGPSTime.IsZero() && time.Since(existing.LastGPSTime) > 60*time.Second {
		existing.OperatorLatitude = 0
		existing.OperatorLongitude = 0
	}
	if d.Registration != "" {
		existing.Registration = d.Registration
	}
	if d.DetectionSource != "" {
		existing.DetectionSource = d.DetectionSource
	}
	if d.TrustScore != 0 {
		existing.TrustScore = d.TrustScore
	}
	if len(d.SpoofFlags) > 0 {
		existing.SpoofFlags = d.SpoofFlags
	}
	if d.Classification != "" {
		existing.Classification = d.Classification
	}

	slog.Debug("Merged drone data",
		"serial", d.SerialNumber,
		"existing_id", existing.Identifier,
		"new_id", d.Identifier,
		"wifi_only", isWifiOnly)
}

// UpdateTap updates or creates a tap in the state
func (s *StateManager) UpdateTap(t *Tap) {
	s.tapMu.Lock()
	defer s.tapMu.Unlock()

	// Carry forward seen channels from existing tap and add current
	if existing, ok := s.taps[t.ID]; ok && existing.SeenChannels != nil {
		t.SeenChannels = existing.SeenChannels
	}
	if t.CurrentChannel > 0 {
		found := false
		for _, ch := range t.SeenChannels {
			if ch == t.CurrentChannel {
				found = true
				break
			}
		}
		if !found {
			t.SeenChannels = append(t.SeenChannels, t.CurrentChannel)
		}
	}

	t.Status = "online"
	t.LastSeen = time.Now()
	s.taps[t.ID] = t

	s.broadcast(StateEvent{Type: "tap_status", Data: t})
}

// GetDrone returns a drone by identifier
func (s *StateManager) GetDrone(id string) (*Drone, bool) {
	return s.drones.get(id)
}

// GetDroneByMAC returns a drone by MAC address, or nil if not found
func (s *StateManager) GetDroneByMAC(mac string) *Drone {
	return s.drones.getByMAC(mac)
}

// GetAllDrones returns all drones
func (s *StateManager) GetAllDrones() []*Drone {
	return s.drones.getAll()
}

// GetActiveDrones returns only active (not lost) drones
func (s *StateManager) GetActiveDrones() []*Drone {
	result := make([]*Drone, 0)
	s.drones.forEach(func(d *Drone) {
		if d.Status == "active" {
			result = append(result, d)
		}
	})
	return result
}

// GetAllTaps returns all taps
func (s *StateManager) GetAllTaps() []*Tap {
	s.tapMu.RLock()
	defer s.tapMu.RUnlock()

	result := make([]*Tap, 0, len(s.taps))
	for _, t := range s.taps {
		result = append(result, t)
	}
	return result
}

// GetTap returns a specific tap by ID
func (s *StateManager) GetTap(id string) (*Tap, bool) {
	s.tapMu.RLock()
	defer s.tapMu.RUnlock()

	t, ok := s.taps[id]
	return t, ok
}

// GetStats returns summary statistics
func (s *StateManager) GetStats() map[string]interface{} {
	activeCount := 0
	lostCount := 0
	lowTrustCount := 0

	s.drones.forEach(func(d *Drone) {
		if d.Status == "active" {
			activeCount++
		} else {
			lostCount++
		}
		if d.TrustScore < 50 {
			lowTrustCount++
		}
	})

	s.tapMu.RLock()
	onlineTaps := 0
	for _, t := range s.taps {
		if t.Status == "online" {
			onlineTaps++
		}
	}
	tapTotal := len(s.taps)
	s.tapMu.RUnlock()

	// Get trail count
	trailCount := 0
	if s.trails != nil {
		trailCount = s.trails.Count()
	}

	return map[string]interface{}{
		"drones_active":   activeCount,
		"drones_lost":     lostCount,
		"drones_total":    s.drones.count(),
		"low_trust_count": lowTrustCount,
		"taps_online":     onlineTaps,
		"taps_total":      tapTotal,
		"trails_active":   trailCount,
	}
}

// StartCleanup starts the background cleanup routine
func (s *StateManager) StartCleanup(ctx context.Context, lostThreshold, tapTimeout time.Duration) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanup(lostThreshold, tapTimeout)
		}
	}
}

func (s *StateManager) cleanup(droneThreshold, tapThreshold time.Duration) {
	now := time.Now()

	// Mark drones as lost (iterate each shard with write lock)
	s.drones.forEachMut(func(d *Drone) {
		if d.Status == "active" && now.Sub(d.LastSeen) > droneThreshold {
			d.Status = "lost"
			d.ContactStatus = "lost"
			slog.Info("Drone lost", "identifier", d.Identifier, "last_seen", d.LastSeen)
			s.broadcast(StateEvent{Type: "drone_lost", Data: d})
		}
	})

	// Evict lost drones that exceed the TTL to prevent unbounded memory growth
	// EvictAfterMin=0 disables eviction (keep all drones in memory)
	evictAfter := time.Duration(s.cfg.EvictAfterMin) * time.Minute
	if s.cfg.EvictAfterMin <= 0 {
		evictAfter = 0
	}
	if evictAfter > 0 {
		var evictIDs []string
		for i := 0; i < numDroneShards; i++ {
			shard := s.drones.shards[i]
			shard.mu.RLock()
			for id, d := range shard.drones {
				if d.Status == "lost" && now.Sub(d.LastSeen) > evictAfter {
					evictIDs = append(evictIDs, id)
				}
			}
			shard.mu.RUnlock()
		}
		for _, id := range evictIDs {
			s.drones.delete(id)
			if s.trails != nil {
				s.trails.RemoveTrail(id)
			}
			slog.Debug("Evicted stale lost drone", "identifier", id)
		}
	}

	// Mark taps as offline
	s.tapMu.Lock()
	for id, t := range s.taps {
		if t.Status == "online" && now.Sub(t.LastSeen) > tapThreshold {
			t.Status = "offline"
			slog.Warn("Tap offline", "id", id, "name", t.Name)
			s.broadcast(StateEvent{Type: "tap_status", Data: t})
		}
	}
	s.tapMu.Unlock()

	// Clean up stale suspect candidates (90s timeout - gives time for channel hopping)
	suspectTimeout := 90 * time.Second
	s.suspectMu.Lock()
	for mac, candidate := range s.suspectCandidates {
		if now.Sub(candidate.LastSeen) > suspectTimeout {
			slog.Debug("Suspect candidate expired",
				"mac", mac,
				"observations", candidate.Observations,
				"taps_seen", len(candidate.TapsSeen),
			)
			delete(s.suspectCandidates, mac)
		}
	}
	s.suspectMu.Unlock()
}

// SetDroneTag sets the tag for a drone
func (s *StateManager) SetDroneTag(id, tag string) bool {
	shard := s.drones.getShard(id)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	if d, ok := shard.drones[id]; ok {
		d.Tag = tag
		s.broadcast(StateEvent{Type: "drone_update", Data: d})
		return true
	}
	return false
}

// SetDroneClassification sets the classification for a drone
func (s *StateManager) SetDroneClassification(id, classification string) bool {
	shard := s.drones.getShard(id)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	if d, ok := shard.drones[id]; ok {
		d.Classification = classification
		s.broadcast(StateEvent{Type: "drone_update", Data: d})
		return true
	}
	return false
}

// DeleteDrone removes a drone from state
func (s *StateManager) DeleteDrone(id string) bool {
	return s.drones.delete(id)
}

// SetDroneHidden sets the hidden flag for a drone
func (s *StateManager) SetDroneHidden(id string, hidden bool) bool {
	shard := s.drones.getShard(id)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	if d, ok := shard.drones[id]; ok {
		d.Hidden = hidden
		s.broadcast(StateEvent{Type: "drone_update", Data: d})
		return true
	}
	return false
}

// HideAllLost hides all lost drones
func (s *StateManager) HideAllLost() {
	s.drones.forEachMut(func(d *Drone) {
		if d.Status == "lost" || d.ContactStatus == "lost" {
			d.Hidden = true
		}
	})
}

// UnhideAll unhides all drones
func (s *StateManager) UnhideAll() {
	s.drones.forEachMut(func(d *Drone) {
		d.Hidden = false
	})
}

// GetVisibleDrones returns all non-hidden drones
func (s *StateManager) GetVisibleDrones() []*Drone {
	result := make([]*Drone, 0)
	s.drones.forEach(func(d *Drone) {
		if !d.Hidden {
			result = append(result, d)
		}
	})
	return result
}

// UpdateDroneRangeRing adds or updates a range ring for a drone from a specific tap
func (s *StateManager) UpdateDroneRangeRing(droneID string, ring RangeRing) bool {
	shard := s.drones.getShard(droneID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	d, ok := shard.drones[droneID]
	if !ok {
		return false
	}

	// Stamp the update time for cleanup
	ring.UpdatedAt = time.Now()

	// Find and update existing ring for this tap, or append new one
	found := false
	for i := range d.RangeRings {
		if d.RangeRings[i].TapID == ring.TapID {
			d.RangeRings[i] = ring
			found = true
			break
		}
	}
	if !found {
		d.RangeRings = append(d.RangeRings, ring)
	}

	// Update DistanceEstM with the best estimate (highest confidence)
	if len(d.RangeRings) > 0 {
		bestRing := d.RangeRings[0]
		for _, r := range d.RangeRings[1:] {
			if r.Confidence > bestRing.Confidence {
				bestRing = r
			}
		}
		d.DistanceEstM = bestRing.DistanceM
	}

	return true
}

// SetDroneEstimatedPosition sets the trilaterated position for a drone.
// WiFi-only drones keep lat=0, lon=0; the map reads EstimatedPos directly.
func (s *StateManager) SetDroneEstimatedPosition(droneID string, pos *EstimatedPosition) bool {
	shard := s.drones.getShard(droneID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	d, ok := shard.drones[droneID]
	if !ok {
		return false
	}

	d.EstimatedPos = pos
	// No promotion to d.Latitude/d.Longitude — WiFi-only drones stay at 0,0.
	// Map reads EstimatedPos directly for multi-TAP display, or shows
	// range rings for single-TAP. Promoting caused stale coordinates that
	// made subsequent estimates silently ignored (lat!=0 after first set).
	return true
}

// GetDroneEstimatedPosition returns the current estimated position for a drone (or nil)
func (s *StateManager) GetDroneEstimatedPosition(droneID string) *EstimatedPosition {
	shard := s.drones.getShard(droneID)
	shard.mu.RLock()
	defer shard.mu.RUnlock()

	d, ok := shard.drones[droneID]
	if !ok || d.EstimatedPos == nil {
		return nil
	}
	// Return a copy
	ep := *d.EstimatedPos
	return &ep
}

// GetDroneRangeRings returns all range rings for a drone
func (s *StateManager) GetDroneRangeRings(droneID string) []RangeRing {
	shard := s.drones.getShard(droneID)
	shard.mu.RLock()
	defer shard.mu.RUnlock()

	d, ok := shard.drones[droneID]
	if !ok {
		return nil
	}

	// Return a copy to avoid race conditions
	rings := make([]RangeRing, len(d.RangeRings))
	copy(rings, d.RangeRings)
	return rings
}

// ClearOldRangeRings removes range rings older than maxAge across ALL drones.
// Should be called periodically (every 5 min) to clean stale RSSI data.
func (s *StateManager) ClearOldRangeRings(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	s.drones.forEachMut(func(d *Drone) {
		if len(d.RangeRings) == 0 {
			return
		}
		kept := d.RangeRings[:0]
		for _, ring := range d.RangeRings {
			if ring.UpdatedAt.IsZero() || ring.UpdatedAt.After(cutoff) {
				kept = append(kept, ring)
			}
		}
		d.RangeRings = kept
	})
}

// =============================================================================
// Suspect Candidate Management
// =============================================================================

// UpdateSuspectCandidate updates or creates a suspect candidate from a WiFi detection
// that does not have RemoteID or positive identification. Returns the correlation result.
// In single-TAP mode, promotion can occur based on:
// - Multiple observations over time (>= minObs, >= minTimeSpan)
// - High mobility score from RSSI variance
// - Manual operator confirmation
func (s *StateManager) UpdateSuspectCandidate(mac string, tapID string, confidence float32, channel int32, rssi int32, flags []string) *CorrelationResult {
	now := time.Now()

	s.suspectMu.Lock()

	candidate, exists := s.suspectCandidates[mac]
	if !exists {
		candidate = &SuspectCandidate{
			Identifier:        "SUSPECT-" + mac,
			MACAddress:        mac,
			FirstSeen:         now,
			LastSeen:          now,
			TapsSeen:          make(map[string]time.Time),
			Observations:      0,
			BestConfidence:    0,
			Channel:           channel,
			AvgRSSI:           0,
			BehavioralFlags:   []string{},
			MobilityScore:     0,
			RemoteIDUpgrade:   false,
			RSSIMin:           rssi,
			RSSIMax:           rssi,
			RSSIVariance:      0,
			ChannelChanges:    0,
			IsLikelyMobile:    false,
			ManuallyConfirmed: false,
		}
		s.suspectCandidates[mac] = candidate
		slog.Info("New suspect candidate",
			"mac", mac,
			"tap_id", tapID,
			"channel", channel,
		)
	}

	// Track channel changes (indicator of mobility)
	if candidate.Channel != channel && candidate.Observations > 0 {
		candidate.ChannelChanges++
	}

	// Update candidate state
	candidate.LastSeen = now
	candidate.TapsSeen[tapID] = now
	candidate.Observations++
	if confidence > candidate.BestConfidence {
		candidate.BestConfidence = confidence
	}
	candidate.Channel = channel

	// Track RSSI min/max for variance calculation
	if rssi < candidate.RSSIMin {
		candidate.RSSIMin = rssi
	}
	if rssi > candidate.RSSIMax {
		candidate.RSSIMax = rssi
	}

	// Update running average RSSI
	if candidate.AvgRSSI == 0 {
		candidate.AvgRSSI = float64(rssi)
	} else {
		// Exponential moving average with alpha=0.3
		candidate.AvgRSSI = 0.3*float64(rssi) + 0.7*candidate.AvgRSSI
	}

	// Compute RSSI variance (high variance = likely moving target)
	// Using range as a simple variance proxy
	rssiRange := float64(candidate.RSSIMax - candidate.RSSIMin)
	candidate.RSSIVariance = rssiRange

	// Merge behavioral flags
	for _, flag := range flags {
		found := false
		for _, existing := range candidate.BehavioralFlags {
			if existing == flag {
				found = true
				break
			}
		}
		if !found {
			candidate.BehavioralFlags = append(candidate.BehavioralFlags, flag)
		}
	}

	// Calculate mobility score - works for both single-TAP and multi-TAP modes
	// Components:
	// 1. TAP diversity (multi-TAP): 0.4 weight
	// 2. RSSI variance (single-TAP capable): 0.3 weight
	// 3. Channel changes (single-TAP capable): 0.2 weight
	// 4. Observation density over time (single-TAP capable): 0.1 weight
	tapDiversityScore := float64(len(candidate.TapsSeen)) / 5.0
	if tapDiversityScore > 1.0 {
		tapDiversityScore = 1.0
	}

	// RSSI variance score: 10dB range = 0.5, 20dB+ range = 1.0
	rssiVarScore := rssiRange / 20.0
	if rssiVarScore > 1.0 {
		rssiVarScore = 1.0
	}

	// Channel change score: 2+ changes = 1.0
	chanChangeScore := float64(candidate.ChannelChanges) / 2.0
	if chanChangeScore > 1.0 {
		chanChangeScore = 1.0
	}

	// Observation density: observations per second (normalized)
	timeSpan := candidate.LastSeen.Sub(candidate.FirstSeen).Seconds()
	var obsPerSec float64
	if timeSpan > 1 {
		obsPerSec = float64(candidate.Observations) / timeSpan
	}
	obsDensityScore := obsPerSec / 2.0 // 2 obs/sec = max score
	if obsDensityScore > 1.0 {
		obsDensityScore = 1.0
	}

	// Combine scores with weights
	if len(candidate.TapsSeen) > 1 {
		// Multi-TAP: weight TAP diversity higher
		candidate.MobilityScore = 0.4*tapDiversityScore + 0.3*rssiVarScore + 0.2*chanChangeScore + 0.1*obsDensityScore
	} else {
		// Single-TAP: weight RSSI variance and channel changes higher
		candidate.MobilityScore = 0.1*tapDiversityScore + 0.45*rssiVarScore + 0.30*chanChangeScore + 0.15*obsDensityScore
	}

	// Determine if likely mobile (threshold check)
	// For single-TAP mode: consider mobile if RSSI variance is high
	candidate.IsLikelyMobile = candidate.MobilityScore >= s.cfg.SingleTapMinMobility ||
		rssiRange >= 15 || // 15dB swing suggests movement
		candidate.ChannelChanges >= 2 // Multiple channel changes

	s.suspectMu.Unlock()

	// Run correlator to check if this candidate should be promoted
	// Pass the candidate for single-TAP mode evaluation
	return s.correlator.AddObservationWithCandidate(mac, tapID, rssi, channel, confidence, candidate, s.cfg)
}

// GetSuspectCandidates returns all current suspect candidates
func (s *StateManager) GetSuspectCandidates() []*SuspectCandidate {
	s.suspectMu.RLock()
	defer s.suspectMu.RUnlock()

	result := make([]*SuspectCandidate, 0, len(s.suspectCandidates))
	for _, c := range s.suspectCandidates {
		result = append(result, c)
	}
	return result
}

// GetSuspectCandidate returns a specific suspect candidate by MAC address
func (s *StateManager) GetSuspectCandidate(mac string) *SuspectCandidate {
	s.suspectMu.RLock()
	defer s.suspectMu.RUnlock()
	return s.suspectCandidates[mac]
}

// PromoteSuspect promotes a suspect candidate to a full drone tracking entry.
// This is called when correlation confirms the suspect is a real drone.
// manufacturer and model are optional - if empty, they default to "UNKNOWN".
func (s *StateManager) PromoteSuspect(mac string, manufacturer, model string) (*Drone, error) {
	s.suspectMu.Lock()
	candidate, exists := s.suspectCandidates[mac]
	if !exists {
		s.suspectMu.Unlock()
		return nil, fmt.Errorf("suspect candidate not found: %s", mac)
	}
	// Remove from suspects
	delete(s.suspectCandidates, mac)
	s.suspectMu.Unlock()

	// Apply defaults for unknown manufacturer/model
	if manufacturer == "" {
		manufacturer = "UNKNOWN"
	}
	if model == "" {
		model = "WiFi Correlated"
	}

	// Get the first tap_id from TapsSeen for attribution
	var firstTapID string
	for tapID := range candidate.TapsSeen {
		firstTapID = tapID
		break
	}

	// Create drone from candidate
	drone := &Drone{
		Identifier:      candidate.Identifier,
		MACAddress:      mac,
		Manufacturer:    manufacturer,
		Model:           model,
		Designation:     "CORRELATED",
		DetectionSource: "WIFI_CORRELATED",
		RSSI:            int32(candidate.AvgRSSI),
		Channel:         candidate.Channel,
		Confidence:      candidate.BestConfidence,
		TrustScore:      50, // Start with moderate trust (no RemoteID)
		Classification:  "UNVERIFIED",
		Status:          "active",
		ContactStatus:   "active",
		FirstSeen:       candidate.FirstSeen,
		LastSeen:        candidate.LastSeen,
		Timestamp:       candidate.LastSeen,
		DetectionCount:  int64(candidate.Observations),
		TapID:           firstTapID,
	}

	// Add spoof flag for missing RemoteID
	drone.SpoofFlags = []string{"NO_REMOTEID"}

	// Update drone state
	_, _ = s.UpdateDrone(drone)

	slog.Info("Suspect promoted to drone",
		"mac", mac,
		"identifier", drone.Identifier,
		"observations", candidate.Observations,
		"taps_seen", len(candidate.TapsSeen),
		"manufacturer", manufacturer,
		"model", model,
	)

	s.broadcast(StateEvent{Type: "suspect_promoted", Data: drone})

	return drone, nil
}

// DismissSuspect removes a suspect candidate from tracking
// Used when a suspect is determined to be non-drone (interference, etc)
func (s *StateManager) DismissSuspect(mac string) {
	s.suspectMu.Lock()
	defer s.suspectMu.Unlock()

	if candidate, exists := s.suspectCandidates[mac]; exists {
		slog.Info("Suspect dismissed",
			"mac", mac,
			"observations", candidate.Observations,
			"taps_seen", len(candidate.TapsSeen),
		)
		delete(s.suspectCandidates, mac)
	}
}

// ConfirmSuspect marks a suspect as manually confirmed by an operator.
// Returns the promoted drone if successful, nil if suspect not found.
// This allows single-TAP deployments to manually promote suspects.
func (s *StateManager) ConfirmSuspect(mac string) (*Drone, error) {
	s.suspectMu.Lock()
	candidate, exists := s.suspectCandidates[mac]
	if !exists {
		s.suspectMu.Unlock()
		return nil, fmt.Errorf("suspect not found: %s", mac)
	}
	candidate.ManuallyConfirmed = true
	s.suspectMu.Unlock()

	slog.Info("Suspect manually confirmed by operator",
		"mac", mac,
		"observations", candidate.Observations,
		"taps_seen", len(candidate.TapsSeen),
	)

	// Promote with operator-confirmed designation
	return s.PromoteSuspectWithConfidence(mac, "UNKNOWN", "Operator Confirmed", 0.85, "OPERATOR_CONFIRMED")
}

// PromoteSuspectWithConfidence promotes a suspect with specified confidence and source
func (s *StateManager) PromoteSuspectWithConfidence(mac string, manufacturer, model string, confidence float32, source string) (*Drone, error) {
	s.suspectMu.Lock()
	candidate, exists := s.suspectCandidates[mac]
	if !exists {
		s.suspectMu.Unlock()
		return nil, fmt.Errorf("suspect candidate not found: %s", mac)
	}
	// Remove from suspects
	delete(s.suspectCandidates, mac)
	s.suspectMu.Unlock()

	// Apply defaults for unknown manufacturer/model
	if manufacturer == "" {
		manufacturer = "UNKNOWN"
	}
	if model == "" {
		model = "WiFi Detected"
	}

	// Determine trust score based on promotion source
	trustScore := 50
	classification := "UNVERIFIED"
	switch source {
	case "MULTI_TAP_CORRELATED":
		trustScore = 70
		classification = "CORRELATED"
	case "OPERATOR_CONFIRMED":
		trustScore = 80
		classification = "OPERATOR_CONFIRMED"
	case "REMOTEID_UPGRADE":
		trustScore = 85
		classification = "VERIFIED"
	case "SINGLE_TAP_MOBILITY":
		trustScore = 55
		classification = "MOBILITY_DETECTED"
	case "SINGLE_TAP_OBSERVATION":
		trustScore = 45
		classification = "OBSERVATION_BASED"
	default:
		trustScore = 50
		classification = "UNVERIFIED"
	}

	// Get the first tap_id from TapsSeen for attribution
	var firstTapID string
	for tapID := range candidate.TapsSeen {
		firstTapID = tapID
		break
	}

	// Create drone from candidate
	drone := &Drone{
		Identifier:      candidate.Identifier,
		MACAddress:      mac,
		Manufacturer:    manufacturer,
		Model:           model,
		Designation:     source,
		DetectionSource: source,
		RSSI:            int32(candidate.AvgRSSI),
		Channel:         candidate.Channel,
		Confidence:      confidence,
		TrustScore:      trustScore,
		Classification:  classification,
		Status:          "active",
		ContactStatus:   "active",
		FirstSeen:       candidate.FirstSeen,
		LastSeen:        candidate.LastSeen,
		Timestamp:       candidate.LastSeen,
		DetectionCount:  int64(candidate.Observations),
		TapID:           firstTapID,
	}

	// Add spoof flags
	drone.SpoofFlags = []string{"NO_REMOTEID"}
	if len(candidate.TapsSeen) == 1 {
		drone.SpoofFlags = append(drone.SpoofFlags, "SINGLE_TAP_ONLY")
	}

	// Update drone state
	_, _ = s.UpdateDrone(drone)

	slog.Info("Suspect promoted to drone",
		"mac", mac,
		"identifier", drone.Identifier,
		"source", source,
		"observations", candidate.Observations,
		"taps_seen", len(candidate.TapsSeen),
		"confidence", confidence,
		"trust_score", trustScore,
	)

	s.broadcast(StateEvent{Type: "suspect_promoted", Data: drone})

	return drone, nil
}

// CheckRemoteIDUpgrade checks if a detected drone matches a suspect candidate
// by MAC address and marks the candidate for upgrade. Returns the matching
// candidate if found, allowing the caller to merge data.
func (s *StateManager) CheckRemoteIDUpgrade(drone *Drone) *SuspectCandidate {
	if drone.MACAddress == "" {
		return nil
	}

	s.suspectMu.Lock()
	defer s.suspectMu.Unlock()

	candidate, exists := s.suspectCandidates[drone.MACAddress]
	if !exists {
		return nil
	}

	// Mark as upgraded - the suspect was later identified via RemoteID
	candidate.RemoteIDUpgrade = true

	slog.Info("Suspect upgraded via RemoteID",
		"mac", drone.MACAddress,
		"drone_id", drone.Identifier,
		"prior_observations", candidate.Observations,
	)

	// Remove from suspects (it's now a confirmed drone)
	delete(s.suspectCandidates, drone.MACAddress)

	return candidate
}

// GetCorrelator returns the suspect correlator for direct access
func (s *StateManager) GetCorrelator() *SuspectCorrelator {
	return s.correlator
}

// GetSuspectStats returns statistics about suspect tracking
func (s *StateManager) GetSuspectStats() map[string]interface{} {
	s.suspectMu.RLock()
	defer s.suspectMu.RUnlock()

	totalObs := 0
	multiTapCount := 0
	for _, c := range s.suspectCandidates {
		totalObs += c.Observations
		if len(c.TapsSeen) > 1 {
			multiTapCount++
		}
	}

	return map[string]interface{}{
		"suspect_count":      len(s.suspectCandidates),
		"total_observations": totalObs,
		"multi_tap_suspects": multiTapCount,
	}
}

// =============================================================================
// Trail Management Methods
// =============================================================================

// GetDroneTrail returns the GPS trail for a specific drone
func (s *StateManager) GetDroneTrail(identifier string) []PositionSample {
	if s.trails == nil {
		return nil
	}
	return s.trails.GetTrail(identifier)
}

// GetDroneTrailSince returns positions after a given time
func (s *StateManager) GetDroneTrailSince(identifier string, since time.Time) []PositionSample {
	if s.trails == nil {
		return nil
	}
	return s.trails.GetTrailSince(identifier, since)
}

// GetAllTrails returns trails for all drones
func (s *StateManager) GetAllTrails() map[string][]PositionSample {
	if s.trails == nil {
		return nil
	}
	return s.trails.GetAllTrails()
}

// GetActiveTrails returns trails with recent activity
func (s *StateManager) GetActiveTrails(maxAge time.Duration) map[string][]PositionSample {
	if s.trails == nil {
		return nil
	}
	return s.trails.GetActiveTrails(maxAge)
}

// GetTrailStats returns statistics for a drone's trail
func (s *StateManager) GetTrailStats(identifier string) *TrailStats {
	if s.trails == nil {
		return nil
	}
	return s.trails.GetTrailStats(identifier)
}

// GetTrailCount returns the number of active trails
func (s *StateManager) GetTrailCount() int {
	if s.trails == nil {
		return 0
	}
	return s.trails.Count()
}

// RemoveDroneTrail removes a trail (called when drone is lost)
func (s *StateManager) RemoveDroneTrail(identifier string) {
	if s.trails != nil {
		s.trails.RemoveTrail(identifier)
	}
}

// GetControllerLink returns the controller link for a given UAV identifier, or nil if not linked.
func (s *StateManager) GetControllerLink(uavID string) *ControllerLink {
	s.controllerMu.RLock()
	defer s.controllerMu.RUnlock()
	for _, link := range s.controllerLinks {
		if link.UAVID == uavID {
			return link
		}
	}
	return nil
}

// GetControllerLinkByController returns the link for a given controller identifier.
func (s *StateManager) GetControllerLinkByController(controllerID string) *ControllerLink {
	s.controllerMu.RLock()
	defer s.controllerMu.RUnlock()
	return s.controllerLinks[controllerID]
}

// SetControllerLink creates or updates a controller-UAV link.
func (s *StateManager) SetControllerLink(link *ControllerLink) {
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()
	s.controllerLinks[link.ControllerID] = link
}

// RemoveControllerLink removes a controller link by controller ID.
func (s *StateManager) RemoveControllerLink(controllerID string) {
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()
	delete(s.controllerLinks, controllerID)
}

// GetAllControllerLinks returns all active controller-UAV links.
func (s *StateManager) GetAllControllerLinks() []*ControllerLink {
	s.controllerMu.RLock()
	defer s.controllerMu.RUnlock()
	links := make([]*ControllerLink, 0, len(s.controllerLinks))
	for _, link := range s.controllerLinks {
		links = append(links, link)
	}
	return links
}
