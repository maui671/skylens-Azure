// Package intel provides intelligence features for drone detection.
// live_calibrator.go implements real-time RSSI calibration from GPS ground truth.
//
// When a drone broadcasts RemoteID with GPS coordinates AND we receive its
// WiFi signal with RSSI, we can compute the actual distance and back-calculate
// the reference power (RSSI_0). Over time, this builds per-model, per-tap
// calibration profiles that are far more accurate than static offsets.
//
// Priority chain for distance estimation:
//   1. Live calibration (this model + this tap, from recent GPS data)
//   2. Live calibration (this model + any tap, aggregated)
//   3. Static per-model TX offsets (from Nzyme field data)
//   4. Generic conservative fallback
package intel

import (
	"log/slog"
	"math"
	"sync"
	"time"
)

const (
	// Max calibration points per model+tap combo (sliding window)
	maxCalPoints = 200

	// Max age of calibration points before they're pruned
	calPointMaxAge = 24 * time.Hour

	// Minimum distance for valid calibration point (too close = near-field effects)
	calMinDistanceM = 50.0

	// Maximum distance for calibration (beyond this, noise floor dominates)
	calMaxDistanceM = 15000.0

	// Minimum calibration points needed before we trust live data
	calMinPoints = 5

	// RSSI sanity bounds for calibration
	calRSSIMin = -120.0
	calRSSIMax = -25.0

	// Maximum identity entries to prevent unbounded memory growth.
	// Each drone stores ~2 entries (identifier + MAC). At 10k entries
	// with ~100 bytes each, this caps memory at ~1MB.
	maxIdentities = 10000
)

// CalibrationPoint is a single GPS+RSSI measurement pair
type CalibrationPoint struct {
	RSSI      float64   // Measured RSSI (dBm)
	DistanceM float64   // Actual distance from haversine (meters)
	RSSI0     float64   // Back-calculated RSSI_0 = RSSI + 10*n*log10(d)
	TapID     string    // Which tap recorded this
	Timestamp time.Time // When this was recorded
}

// ModelCalibration holds live calibration data for a specific drone model
type ModelCalibration struct {
	Model        string              // Drone model name
	Points       []CalibrationPoint  // Sliding window of calibration points
	TapPoints    map[string][]CalibrationPoint // Per-tap calibration points
	LiveRSSI0    float64             // Fitted RSSI_0 from live data (all taps)
	TapRSSI0     map[string]float64  // Per-tap fitted RSSI_0
	PointCount   int                 // Total points collected
	LastUpdated  time.Time           // Last calibration update
	MeanErrorPct float64             // Cross-validated mean error %
}

// IdentityMemory remembers which identifier/MAC maps to which model
// so we can identify drones even after they kill RemoteID
type IdentityMemory struct {
	Model      string    // Drone model identified while RemoteID was active
	Serial     string    // Serial number (if known)
	LastSeen   time.Time // Last time we confirmed this identity
}

// LiveCalibrator collects GPS+RSSI pairs and builds per-model calibration profiles
type LiveCalibrator struct {
	mu           sync.RWMutex
	models       map[string]*ModelCalibration // key: model name
	identities   map[string]*IdentityMemory   // key: drone identifier or MAC
	pathLossN    float64                       // Path loss exponent (usually 1.8-2.0)
}

// NewLiveCalibrator creates a new live RSSI calibrator
func NewLiveCalibrator(pathLossN float64) *LiveCalibrator {
	if pathLossN == 0 {
		pathLossN = DefaultPathLossN
	}
	return &LiveCalibrator{
		models:     make(map[string]*ModelCalibration),
		identities: make(map[string]*IdentityMemory),
		pathLossN:  pathLossN,
	}
}

// RecordCalibrationPoint records a GPS+RSSI pair for live calibration.
// Called when we see a drone with both GPS (from RemoteID) and RSSI.
func (lc *LiveCalibrator) RecordCalibrationPoint(model, tapID string, rssi, distanceM float64) {
	// Validate inputs
	if model == "" || tapID == "" {
		return
	}

	// Apply per-TAP RSSI calibration offset BEFORE computing RSSI_0.
	// This matches EstimateDistanceLive which also applies CalibrateRSSI before estimation.
	// Without this, TAPs with offsets (e.g. tap-003 +16dB) produce RSSI_0 values that
	// are inconsistent with the estimation path, causing ~87% distance underestimation.
	rssi = CalibrateRSSI(rssi, tapID)

	if rssi < calRSSIMin || rssi > calRSSIMax || rssi == RSSIPlaceholder {
		return
	}
	if distanceM < calMinDistanceM || distanceM > calMaxDistanceM {
		return
	}

	// Back-calculate RSSI_0 from this measurement using tap's environment n
	n := GetTapEnvironment(tapID).PathLossExponent()
	rssi0 := ReverseCalibrate(rssi, distanceM, n)

	now := time.Now()
	point := CalibrationPoint{
		RSSI:      rssi,
		DistanceM: distanceM,
		RSSI0:     rssi0,
		TapID:     tapID,
		Timestamp: now,
	}

	lc.mu.Lock()
	defer lc.mu.Unlock()

	cal, exists := lc.models[model]
	if !exists {
		cal = &ModelCalibration{
			Model:     model,
			Points:    make([]CalibrationPoint, 0, maxCalPoints),
			TapPoints: make(map[string][]CalibrationPoint),
			TapRSSI0:  make(map[string]float64),
		}
		lc.models[model] = cal
	}

	// Add to global sliding window
	cal.Points = append(cal.Points, point)
	if len(cal.Points) > maxCalPoints {
		cal.Points = cal.Points[len(cal.Points)-maxCalPoints:]
	}

	// Add to per-tap sliding window
	tapPoints := cal.TapPoints[tapID]
	tapPoints = append(tapPoints, point)
	if len(tapPoints) > maxCalPoints {
		tapPoints = tapPoints[len(tapPoints)-maxCalPoints:]
	}
	cal.TapPoints[tapID] = tapPoints

	cal.PointCount++
	cal.LastUpdated = now

	// Refit RSSI_0 if we have enough points
	if len(cal.Points) >= calMinPoints {
		cal.LiveRSSI0 = lc.fitRSSI0(cal.Points)
	}
	if len(cal.TapPoints[tapID]) >= calMinPoints {
		cal.TapRSSI0[tapID] = lc.fitRSSI0(cal.TapPoints[tapID])
	}

	slog.Debug("Live calibration point recorded",
		"model", model,
		"tap", tapID,
		"rssi", rssi,
		"distance_m", distanceM,
		"rssi0", rssi0,
		"total_points", cal.PointCount,
	)
}

// RememberIdentity stores the model identity for a drone identifier/MAC.
// When the drone later kills RemoteID, we can still identify what it is.
// Enforces maxIdentities cap - evicts oldest entries when full.
func (lc *LiveCalibrator) RememberIdentity(identifier, model, serial string) {
	if identifier == "" || model == "" {
		return
	}

	lc.mu.Lock()
	defer lc.mu.Unlock()

	// If this identifier is already known, just update it (no growth)
	if existing, ok := lc.identities[identifier]; ok {
		existing.Model = model
		existing.Serial = serial
		existing.LastSeen = time.Now()
		return
	}

	// Enforce max size - evict oldest entries if at capacity
	if len(lc.identities) >= maxIdentities {
		lc.evictOldestIdentities(maxIdentities / 10) // Evict 10% to avoid evicting on every call
	}

	lc.identities[identifier] = &IdentityMemory{
		Model:    model,
		Serial:   serial,
		LastSeen: time.Now(),
	}
}

// evictOldestIdentities removes the N oldest identity entries.
// Must be called with lc.mu held.
func (lc *LiveCalibrator) evictOldestIdentities(count int) {
	if count <= 0 || len(lc.identities) == 0 {
		return
	}

	// Find the N oldest entries by LastSeen
	type idAge struct {
		id       string
		lastSeen time.Time
	}
	entries := make([]idAge, 0, len(lc.identities))
	for id, mem := range lc.identities {
		entries = append(entries, idAge{id: id, lastSeen: mem.LastSeen})
	}

	// Simple selection: find oldest N using partial sort
	for i := 0; i < count && i < len(entries); i++ {
		minIdx := i
		for j := i + 1; j < len(entries); j++ {
			if entries[j].lastSeen.Before(entries[minIdx].lastSeen) {
				minIdx = j
			}
		}
		entries[i], entries[minIdx] = entries[minIdx], entries[i]
		delete(lc.identities, entries[i].id)
	}

	slog.Debug("Evicted oldest identity entries",
		"evicted", count,
		"remaining", len(lc.identities),
	)
}

// RecallModel returns the remembered model for an identifier.
// Used when a drone is detected by WiFi only (no RemoteID).
func (lc *LiveCalibrator) RecallModel(identifier string) (model string, serial string, ok bool) {
	lc.mu.RLock()
	defer lc.mu.RUnlock()

	if mem, exists := lc.identities[identifier]; exists {
		// Only recall if the identity is recent (within 24 hours)
		if time.Since(mem.LastSeen) < calPointMaxAge {
			return mem.Model, mem.Serial, true
		}
	}
	return "", "", false
}

// GetCalibratedRSSI0 returns the best RSSI_0 for a model, using live data if available.
// Priority: tap-specific live > all-tap live > static offset > generic
func (lc *LiveCalibrator) GetCalibratedRSSI0(model, tapID string) (rssi0 float64, source string) {
	lc.mu.RLock()
	defer lc.mu.RUnlock()

	if model != "" {
		if cal, exists := lc.models[model]; exists {
			// Priority 1: Live calibration for this specific tap
			if tapID != "" {
				if tapRSSI0, ok := cal.TapRSSI0[tapID]; ok && len(cal.TapPoints[tapID]) >= calMinPoints {
					return tapRSSI0, "live_tap"
				}
			}

			// Priority 2: Live calibration aggregated across all taps
			if len(cal.Points) >= calMinPoints {
				return cal.LiveRSSI0, "live_all"
			}
		}
	}

	// Priority 3: Static per-model offset
	if model != "" {
		if offset, ok := ModelTXOffsets[model]; ok {
			return BaseRSSI0 + offset, "static_model"
		}
	}

	// Priority 4: Generic fallback
	return GenericRSSI0, "generic"
}

// EstimateDistanceLive estimates distance using best available calibration.
// This is the main entry point - replaces static EstimateDistanceFromRSSI.
func (lc *LiveCalibrator) EstimateDistanceLive(rssi float64, model, tapID string) (distanceM float64, confidence float64, source string) {
	if rssi >= 0 || rssi < RSSIMin || rssi > RSSIMax {
		return -1, 0, ""
	}

	// Apply per-TAP RSSI calibration offset (adapter sensitivity correction)
	rssi = CalibrateRSSI(rssi, tapID)

	rssi0, source := lc.GetCalibratedRSSI0(model, tapID)

	// Use per-tap environment path loss exponent when available
	n := GetTapEnvironment(tapID).PathLossExponent()
	exponent := (rssi0 - rssi) / (10 * n)
	distanceM = math.Pow(10, exponent)

	// Clamp
	if distanceM < MinDistanceM {
		distanceM = MinDistanceM
	}
	if distanceM > MaxDistanceM {
		distanceM = MaxDistanceM
	}

	// Confidence based on calibration source
	switch source {
	case "live_tap":
		confidence = 0.85 // Best: live data from this specific tap
	case "live_all":
		confidence = 0.75 // Good: live data aggregated
	case "static_model":
		confidence = 0.60 // OK: known model but static offsets
	default:
		confidence = 0.35 // Low: generic estimate
	}

	return math.Round(distanceM*10) / 10, confidence, source
}

// EstimateDistanceLiveWithBounds returns distance estimate with uncertainty bounds.
func (lc *LiveCalibrator) EstimateDistanceLiveWithBounds(rssi float64, model, tapID string) DistanceEstimateWithBounds {
	dist, conf, source := lc.EstimateDistanceLive(rssi, model, tapID)
	if dist < 0 {
		return DistanceEstimateWithBounds{}
	}

	// Tighter bounds for live-calibrated estimates
	var boundsMultiplier float64
	switch source {
	case "live_tap":
		boundsMultiplier = 0.30 // +/- 30% for live tap-specific
	case "live_all":
		boundsMultiplier = 0.40 // +/- 40% for live aggregated
	case "static_model":
		boundsMultiplier = 0.50 // +/- 50% for known model
	default:
		boundsMultiplier = 0.75 // +/- 75% for unknown
	}

	env := GetTapEnvironment(tapID)

	return DistanceEstimateWithBounds{
		DistanceM:    dist,
		DistanceMinM: math.Max(MinDistanceM, dist/(1+boundsMultiplier)),
		DistanceMaxM: math.Min(MaxDistanceM, dist*(1+boundsMultiplier)),
		Confidence:   conf,
		ModelUsed:    formatModelSource(model, source),
		Environment:  env.String(),
		PathLossN:    env.PathLossExponent(),
		SigmaDB:      env.ShadowingSigma(),
	}
}

// fitRSSI0 computes the median RSSI_0 from a set of calibration points.
// Median is more robust than mean against outliers (multipath, etc).
func (lc *LiveCalibrator) fitRSSI0(points []CalibrationPoint) float64 {
	if len(points) == 0 {
		return GenericRSSI0
	}

	// Prune old points
	now := time.Now()
	fresh := make([]float64, 0, len(points))
	for _, p := range points {
		if now.Sub(p.Timestamp) <= calPointMaxAge {
			fresh = append(fresh, p.RSSI0)
		}
	}

	if len(fresh) == 0 {
		return GenericRSSI0
	}

	// Use median for robustness
	return median(fresh)
}

// GetStats returns calibration statistics for monitoring
func (lc *LiveCalibrator) GetStats() map[string]interface{} {
	lc.mu.RLock()
	defer lc.mu.RUnlock()

	modelStats := make([]map[string]interface{}, 0, len(lc.models))
	for model, cal := range lc.models {
		stat := map[string]interface{}{
			"model":        model,
			"total_points": cal.PointCount,
			"window_size":  len(cal.Points),
			"live_rssi0":   math.Round(cal.LiveRSSI0*10) / 10,
			"last_updated": cal.LastUpdated.Format(time.RFC3339),
			"taps":         len(cal.TapRSSI0),
		}

		// Show per-tap RSSI_0
		tapDetails := make(map[string]interface{})
		for tapID, rssi0 := range cal.TapRSSI0 {
			tapDetails[tapID] = map[string]interface{}{
				"rssi0":  math.Round(rssi0*10) / 10,
				"points": len(cal.TapPoints[tapID]),
			}
		}
		stat["tap_calibrations"] = tapDetails

		// Compare with static offset
		if offset, ok := ModelTXOffsets[model]; ok {
			staticRSSI0 := BaseRSSI0 + offset
			stat["static_rssi0"] = staticRSSI0
			if cal.LiveRSSI0 != 0 {
				stat["drift_db"] = math.Round((cal.LiveRSSI0-staticRSSI0)*10) / 10
			}
		}

		modelStats = append(modelStats, stat)
	}

	return map[string]interface{}{
		"models_calibrated": len(lc.models),
		"identities_cached": len(lc.identities),
		"path_loss_n":       lc.pathLossN,
		"models":            modelStats,
	}
}

// PruneStale removes old calibration points and expired identity memories
func (lc *LiveCalibrator) PruneStale() {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	now := time.Now()

	// Prune calibration points
	for model, cal := range lc.models {
		fresh := make([]CalibrationPoint, 0, len(cal.Points))
		for _, p := range cal.Points {
			if now.Sub(p.Timestamp) <= calPointMaxAge {
				fresh = append(fresh, p)
			}
		}
		cal.Points = fresh

		// Prune per-tap points
		for tapID, tapPoints := range cal.TapPoints {
			freshTap := make([]CalibrationPoint, 0, len(tapPoints))
			for _, p := range tapPoints {
				if now.Sub(p.Timestamp) <= calPointMaxAge {
					freshTap = append(freshTap, p)
				}
			}
			if len(freshTap) == 0 {
				delete(cal.TapPoints, tapID)
				delete(cal.TapRSSI0, tapID)
			} else {
				cal.TapPoints[tapID] = freshTap
			}
		}

		// Remove model if no points left
		if len(cal.Points) == 0 {
			delete(lc.models, model)
		}
	}

	// Prune old identity memories (keep for 48 hours)
	for id, mem := range lc.identities {
		if now.Sub(mem.LastSeen) > 48*time.Hour {
			delete(lc.identities, id)
		}
	}
}

// formatModelSource creates a clean model+source label
func formatModelSource(model, source string) string {
	if model == "" {
		return "unknown [" + source + "]"
	}
	return model + " [" + source + "]"
}

// median calculates the median of a float64 slice
func median(values []float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}

	// Copy to avoid mutating original
	sorted := make([]float64, n)
	copy(sorted, values)

	// Simple insertion sort (slice is small)
	for i := 1; i < n; i++ {
		key := sorted[i]
		j := i - 1
		for j >= 0 && sorted[j] > key {
			sorted[j+1] = sorted[j]
			j--
		}
		sorted[j+1] = key
	}

	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}

// HaversineDistance calculates distance between two GPS coordinates in meters
func HaversineDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000 // Earth radius in meters
	phi1 := lat1 * math.Pi / 180
	phi2 := lat2 * math.Pi / 180
	dphi := (lat2 - lat1) * math.Pi / 180
	dlambda := (lon2 - lon1) * math.Pi / 180

	a := math.Sin(dphi/2)*math.Sin(dphi/2) +
		math.Cos(phi1)*math.Cos(phi2)*math.Sin(dlambda/2)*math.Sin(dlambda/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return R * c
}
