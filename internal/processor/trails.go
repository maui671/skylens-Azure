package processor

import (
	"math"
	"sync"
	"time"
)

// PositionSample represents a single GPS position in the trail
type PositionSample struct {
	Time      time.Time `json:"time"`
	Lat       float64   `json:"lat"`
	Lon       float64   `json:"lon"`
	Alt       float32   `json:"alt"`
	AltAGL    float32   `json:"alt_agl,omitempty"`
	Speed     float32   `json:"speed,omitempty"`
	Heading   float32   `json:"heading,omitempty"`
	VSpeed    float32   `json:"vspeed,omitempty"`
	RSSI      int32     `json:"rssi,omitempty"`
	TapID     string    `json:"tap_id,omitempty"`
}

// DroneTrail holds recent position history for real-time visualization
type DroneTrail struct {
	mu           sync.RWMutex
	samples      []PositionSample
	maxSize      int
	minInterval  time.Duration // Minimum time between samples
	minDistanceM float64       // Minimum distance between samples
	lastSample   *PositionSample
}

// TrailConfig configures trail behavior
type TrailConfig struct {
	MaxSize      int           // Maximum positions to keep
	MinInterval  time.Duration // Minimum time between samples
	MinDistanceM float64       // Minimum distance (meters) between samples
}

// DefaultTrailConfig returns sensible defaults
func DefaultTrailConfig() TrailConfig {
	return TrailConfig{
		MaxSize:      200,             // 200 positions per drone
		MinInterval:  500 * time.Millisecond, // Max 2 samples/sec
		MinDistanceM: 1.0,             // 1 meter minimum movement
	}
}

// TrailManager manages GPS trails for all drones
type TrailManager struct {
	mu      sync.RWMutex
	trails  map[string]*DroneTrail
	config  TrailConfig
	maxAge  time.Duration // Auto-cleanup after this duration of inactivity
}

// NewTrailManager creates a new trail manager
func NewTrailManager(config TrailConfig) *TrailManager {
	tm := &TrailManager{
		trails: make(map[string]*DroneTrail),
		config: config,
		maxAge: 10 * time.Minute,
	}

	// Start cleanup goroutine
	go tm.cleanupLoop()

	return tm
}

// RecordPosition adds a position to the drone's trail
func (tm *TrailManager) RecordPosition(identifier string, sample PositionSample) bool {
	if sample.Lat == 0 && sample.Lon == 0 {
		return false
	}

	tm.mu.Lock()
	trail, exists := tm.trails[identifier]
	if !exists {
		trail = &DroneTrail{
			samples:      make([]PositionSample, 0, tm.config.MaxSize),
			maxSize:      tm.config.MaxSize,
			minInterval:  tm.config.MinInterval,
			minDistanceM: tm.config.MinDistanceM,
		}
		tm.trails[identifier] = trail
	}
	tm.mu.Unlock()

	return trail.addSample(sample)
}

// addSample adds a sample to the trail with deduplication
func (t *DroneTrail) addSample(sample PositionSample) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Set timestamp if not provided
	if sample.Time.IsZero() {
		sample.Time = time.Now()
	}

	// Check deduplication
	if t.lastSample != nil {
		// Time check
		if sample.Time.Sub(t.lastSample.Time) < t.minInterval {
			return false
		}

		// Distance check
		dist := haversineMeters(t.lastSample.Lat, t.lastSample.Lon, sample.Lat, sample.Lon)
		if dist < t.minDistanceM && sample.Time.Sub(t.lastSample.Time) < 5*time.Second {
			return false
		}
	}

	// Add sample
	t.samples = append(t.samples, sample)

	// Trim if over capacity
	if len(t.samples) > t.maxSize {
		// Remove oldest 10%
		removeCount := t.maxSize / 10
		t.samples = t.samples[removeCount:]
	}

	t.lastSample = &sample
	return true
}

// GetTrail returns the position trail for a drone
func (tm *TrailManager) GetTrail(identifier string) []PositionSample {
	tm.mu.RLock()
	trail, exists := tm.trails[identifier]
	tm.mu.RUnlock()

	if !exists {
		return nil
	}

	trail.mu.RLock()
	defer trail.mu.RUnlock()

	result := make([]PositionSample, len(trail.samples))
	copy(result, trail.samples)
	return result
}

// GetTrailSince returns positions after a given time
func (tm *TrailManager) GetTrailSince(identifier string, since time.Time) []PositionSample {
	tm.mu.RLock()
	trail, exists := tm.trails[identifier]
	tm.mu.RUnlock()

	if !exists {
		return nil
	}

	trail.mu.RLock()
	defer trail.mu.RUnlock()

	result := make([]PositionSample, 0)
	for _, s := range trail.samples {
		if s.Time.After(since) {
			result = append(result, s)
		}
	}
	return result
}

// GetAllTrails returns trails for all drones
func (tm *TrailManager) GetAllTrails() map[string][]PositionSample {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	result := make(map[string][]PositionSample, len(tm.trails))
	for id, trail := range tm.trails {
		trail.mu.RLock()
		samples := make([]PositionSample, len(trail.samples))
		copy(samples, trail.samples)
		trail.mu.RUnlock()
		result[id] = samples
	}
	return result
}

// GetActiveTrails returns trails with recent activity
func (tm *TrailManager) GetActiveTrails(maxAge time.Duration) map[string][]PositionSample {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	result := make(map[string][]PositionSample)
	cutoff := time.Now().Add(-maxAge)

	for id, trail := range tm.trails {
		trail.mu.RLock()
		if len(trail.samples) > 0 && trail.samples[len(trail.samples)-1].Time.After(cutoff) {
			samples := make([]PositionSample, len(trail.samples))
			copy(samples, trail.samples)
			result[id] = samples
		}
		trail.mu.RUnlock()
	}
	return result
}

// GetTrailStats returns statistics for a trail
func (tm *TrailManager) GetTrailStats(identifier string) *TrailStats {
	tm.mu.RLock()
	trail, exists := tm.trails[identifier]
	tm.mu.RUnlock()

	if !exists || len(trail.samples) == 0 {
		return nil
	}

	trail.mu.RLock()
	defer trail.mu.RUnlock()

	stats := &TrailStats{
		Identifier:    identifier,
		PositionCount: len(trail.samples),
	}

	if len(trail.samples) > 0 {
		stats.FirstSeen = trail.samples[0].Time
		stats.LastSeen = trail.samples[len(trail.samples)-1].Time
		stats.DurationSec = stats.LastSeen.Sub(stats.FirstSeen).Seconds()
	}

	// Calculate distance, max altitude, max speed
	for i, s := range trail.samples {
		if s.Alt > stats.MaxAltitude {
			stats.MaxAltitude = s.Alt
		}
		if s.Speed > stats.MaxSpeed {
			stats.MaxSpeed = s.Speed
		}

		if i > 0 {
			prev := trail.samples[i-1]
			stats.TotalDistanceM += haversineMeters(prev.Lat, prev.Lon, s.Lat, s.Lon)
		}
	}

	return stats
}

// TrailStats contains computed statistics for a trail
type TrailStats struct {
	Identifier     string    `json:"identifier"`
	PositionCount  int       `json:"position_count"`
	FirstSeen      time.Time `json:"first_seen"`
	LastSeen       time.Time `json:"last_seen"`
	DurationSec    float64   `json:"duration_sec"`
	TotalDistanceM float64   `json:"total_distance_m"`
	MaxAltitude    float32   `json:"max_altitude_m"`
	MaxSpeed       float32   `json:"max_speed_ms"`
}

// RemoveTrail removes a trail (call when drone is lost)
func (tm *TrailManager) RemoveTrail(identifier string) {
	tm.mu.Lock()
	delete(tm.trails, identifier)
	tm.mu.Unlock()
}

// ClearAll removes all trails
func (tm *TrailManager) ClearAll() {
	tm.mu.Lock()
	tm.trails = make(map[string]*DroneTrail)
	tm.mu.Unlock()
}

// cleanupLoop periodically removes stale trails
func (tm *TrailManager) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		tm.cleanup()
	}
}

// cleanup removes trails with no recent activity
func (tm *TrailManager) cleanup() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	cutoff := time.Now().Add(-tm.maxAge)
	toRemove := make([]string, 0)

	for id, trail := range tm.trails {
		trail.mu.RLock()
		if len(trail.samples) == 0 || trail.samples[len(trail.samples)-1].Time.Before(cutoff) {
			toRemove = append(toRemove, id)
		}
		trail.mu.RUnlock()
	}

	for _, id := range toRemove {
		delete(tm.trails, id)
	}
}

// Count returns the number of active trails
func (tm *TrailManager) Count() int {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return len(tm.trails)
}

// haversineMeters calculates distance between two points in meters
func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000 // Earth radius in meters

	lat1Rad := lat1 * math.Pi / 180
	lat2Rad := lat2 * math.Pi / 180
	deltaLat := (lat2 - lat1) * math.Pi / 180
	deltaLon := (lon2 - lon1) * math.Pi / 180

	sinDLat := math.Sin(deltaLat / 2)
	sinDLon := math.Sin(deltaLon / 2)
	a := sinDLat*sinDLat + math.Cos(lat1Rad)*math.Cos(lat2Rad)*sinDLon*sinDLon
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return R * c
}
