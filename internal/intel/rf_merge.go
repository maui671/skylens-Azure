// Package intel provides WiFi+RF detection merging logic.
// When the same physical drone is detected via WiFi (RemoteID/NAN/beacon)
// AND via SDR (RF energy/protocol), the merge logic links them into a single
// track, enriching the RF detection with WiFi identity and vice versa.
package intel

import (
	"math"
	"sync"
	"time"
)

// RFMergeEntry tracks an RF detection's correlation with WiFi detections
type RFMergeEntry struct {
	SyntheticID  string    // RF synthetic ID (e.g., "rf:5745000:20mhz")
	WiFiID       string    // Linked WiFi identifier (MAC or serial)
	Confidence   float64   // Merge confidence (0-1)
	LastSeen     time.Time // Last time this correlation was observed
	MatchCount   int       // Number of times this pairing was confirmed
	FreqMHz      float64   // RF center frequency
	BandwidthMHz float64   // RF signal bandwidth
}

// RFMerger correlates RF detections with WiFi detections to merge identities.
// The key insight: if a DJI drone is detected via WiFi RemoteID on channel 6
// AND via SDR on 5745 MHz (OcuSync video), and both detections come from
// the same TAP at similar times with correlated RSSI trends, they're the same drone.
type RFMerger struct {
	mu      sync.RWMutex
	entries map[string]*RFMergeEntry // Key: synthetic RF ID
	reverse map[string]string        // WiFi ID → synthetic RF ID (reverse lookup)
}

// NewRFMerger creates a new RF-WiFi merge correlator
func NewRFMerger() *RFMerger {
	return &RFMerger{
		entries: make(map[string]*RFMergeEntry),
		reverse: make(map[string]string),
	}
}

// TryMerge attempts to correlate an RF detection with a known WiFi detection.
// Criteria for a match:
//  1. Same TAP detected both within a short time window
//  2. RSSI values are correlated (both strong or both weak)
//  3. Frequency band is consistent with known drone protocol
//
// Returns the WiFi identifier if a merge is found, empty string otherwise.
func (m *RFMerger) TryMerge(syntheticID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, ok := m.entries[syntheticID]
	if !ok {
		return ""
	}
	if time.Since(entry.LastSeen) > 5*time.Minute {
		return "" // Stale correlation
	}
	return entry.WiFiID
}

// RecordCorrelation records a confirmed correlation between an RF and WiFi detection.
// Call this when both detections arrive from the same TAP within a short time window
// with correlated RSSI values.
func (m *RFMerger) RecordCorrelation(syntheticID, wifiID string, freqMHz, bwMHz, confidence float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if entry, ok := m.entries[syntheticID]; ok {
		// Update existing
		entry.WiFiID = wifiID
		entry.LastSeen = now
		entry.MatchCount++
		entry.Confidence = math.Min(entry.Confidence+0.05, 0.95) // Grows with repeated confirmation
	} else {
		m.entries[syntheticID] = &RFMergeEntry{
			SyntheticID:  syntheticID,
			WiFiID:       wifiID,
			Confidence:   confidence,
			LastSeen:     now,
			MatchCount:   1,
			FreqMHz:      freqMHz,
			BandwidthMHz: bwMHz,
		}
	}
	m.reverse[wifiID] = syntheticID
}

// GetRFForWiFi returns the RF synthetic ID linked to a WiFi identifier, if any.
func (m *RFMerger) GetRFForWiFi(wifiID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reverse[wifiID]
}

// ShouldCorrelate checks if two detections (one WiFi, one RF) from the same TAP
// are likely the same drone, based on temporal and RSSI correlation.
func ShouldCorrelate(
	wifiTimestampNs, rfTimestampNs int64,
	wifiRSSI, rfPowerDBm int32,
	rfProtocol RFProtocol,
	wifiManufacturer string,
) bool {
	// Time check: must be within 2 seconds
	deltaNs := wifiTimestampNs - rfTimestampNs
	if deltaNs < 0 {
		deltaNs = -deltaNs
	}
	if deltaNs > 2e9 { // 2 seconds
		return false
	}

	// Protocol-manufacturer consistency check
	switch rfProtocol {
	case RFProtoDJIOcuSync, RFProtoDJIDroneID:
		if wifiManufacturer != "DJI" && wifiManufacturer != "" {
			return false // RF says DJI but WiFi says different manufacturer
		}
	}

	// RSSI correlation: both should be roughly similar strength
	// WiFi RSSI and RF power aren't directly comparable (different radios),
	// but if one is -40dBm and the other is -90dBm, they're probably not the same drone
	rssiDiff := int32(math.Abs(float64(wifiRSSI - rfPowerDBm)))
	if rssiDiff > 30 {
		return false // Too different
	}

	return true
}

// Cleanup removes stale entries older than maxAge
func (m *RFMerger) Cleanup(maxAge time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for id, entry := range m.entries {
		if entry.LastSeen.Before(cutoff) {
			delete(m.reverse, entry.WiFiID)
			delete(m.entries, id)
			removed++
		}
	}
	return removed
}

// Stats returns current merge statistics
func (m *RFMerger) Stats() (total, active int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	total = len(m.entries)
	cutoff := time.Now().Add(-5 * time.Minute)
	for _, e := range m.entries {
		if e.LastSeen.After(cutoff) {
			active++
		}
	}
	return
}
