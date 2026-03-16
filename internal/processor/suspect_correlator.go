package processor

import (
	"sync"
	"time"
)

// TapObservation holds a single TAP's observation of a suspect MAC.
type TapObservation struct {
	TapID     string
	Timestamp time.Time
	RSSI      int32
	Channel   int32
}

// CorrelationWindow tracks observations of a single MAC across multiple TAPs.
type CorrelationWindow struct {
	MAC       string
	TapObs    map[string]*TapObservation
	FirstSeen time.Time
	LastSeen  time.Time
}

// PromotionReason indicates why a suspect was promoted
type PromotionReason string

const (
	PromotionNone             PromotionReason = ""
	PromotionMultiTap         PromotionReason = "MULTI_TAP_CORRELATED"
	PromotionSingleTapMobility PromotionReason = "SINGLE_TAP_MOBILITY"
	PromotionSingleTapObservation PromotionReason = "SINGLE_TAP_OBSERVATION"
	PromotionOperatorConfirmed PromotionReason = "OPERATOR_CONFIRMED"
	PromotionRemoteIDUpgrade  PromotionReason = "REMOTEID_UPGRADE"
)

// CorrelationResult indicates whether a suspect should be promoted and why.
// In single-TAP mode, promotion can occur via multiple pathways beyond multi-TAP correlation.
type CorrelationResult struct {
	// Confirmed indicates the suspect should be promoted to full drone tracking
	Confirmed bool

	// TapsCount is the number of unique TAPs that have observed this suspect
	TapsCount int

	// CombinedConfidence is the aggregated confidence score (0.0-1.0)
	CombinedConfidence float32

	// PromotionReason indicates why the suspect was promoted (if Confirmed=true)
	Reason PromotionReason

	// ConfidenceLevel indicates relative certainty: "high", "medium", "low"
	ConfidenceLevel string

	// SingleTapEligible indicates suspect could be promoted via single-TAP criteria
	// but may not yet meet the thresholds (informational for dashboard)
	SingleTapEligible bool

	// Observations is the total observation count
	Observations int

	// TimeSpanSec is the time span from first to last observation in seconds
	TimeSpanSec float64

	// MobilityScore from the candidate (0-1)
	MobilityScore float64

	// IsLikelyMobile indicates the candidate shows mobile behavior
	IsLikelyMobile bool
}

// SuspectCorrelator correlates suspect observations across multiple TAPs
// to boost confidence and confirm detections. In single-TAP mode, it also
// evaluates promotion based on mobility score and observation patterns.
type SuspectCorrelator struct {
	observations    map[string]*CorrelationWindow
	windowDuration  time.Duration
	minTapsRequired int
	confidenceBoost float32
	singleTapMode   bool // When true, enable single-TAP promotion pathways
	mu              sync.RWMutex
}

// NewSuspectCorrelator creates a new correlator with the specified parameters.
// windowSec: sliding window duration in seconds (default 5)
// minTaps: minimum TAPs required for high-confidence confirmation (default 2)
// boost: confidence boost per additional TAP beyond first (default 0.30)
// singleTapMode: when true, enables single-TAP promotion based on mobility/observations
func NewSuspectCorrelator(windowSec int, minTaps int, boost float32, singleTapMode bool) *SuspectCorrelator {
	if windowSec <= 0 {
		windowSec = 5
	}
	if minTaps <= 0 {
		minTaps = 2
	}
	if boost <= 0 {
		boost = 0.30
	}

	return &SuspectCorrelator{
		observations:    make(map[string]*CorrelationWindow),
		windowDuration:  time.Duration(windowSec) * time.Second,
		minTapsRequired: minTaps,
		confidenceBoost: boost,
		singleTapMode:   singleTapMode,
	}
}

// AddObservation records a TAP observation for the given MAC and evaluates correlation.
// Returns a CorrelationResult indicating whether the suspect is confirmed and the combined confidence.
// Note: This is the legacy method. Use AddObservationWithCandidate for single-TAP mode support.
func (sc *SuspectCorrelator) AddObservation(mac, tapID string, rssi int32, channel int32, baseConfidence float32) *CorrelationResult {
	// Call the new method with nil candidate (multi-TAP only mode)
	return sc.AddObservationWithCandidate(mac, tapID, rssi, channel, baseConfidence, nil, DetectionConfig{})
}

// AddObservationWithCandidate records a TAP observation and evaluates both multi-TAP
// and single-TAP promotion criteria. In single-TAP mode, suspects can be promoted based on:
// - High mobility score (IsLikelyMobile = true)
// - Multiple observations over time (>= minObs, >= minTimeSpan)
// - Manual operator confirmation (handled separately via ConfirmSuspect)
func (sc *SuspectCorrelator) AddObservationWithCandidate(mac, tapID string, rssi int32, channel int32, baseConfidence float32, candidate *SuspectCandidate, cfg DetectionConfig) *CorrelationResult {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	now := time.Now()

	window, exists := sc.observations[mac]
	if !exists {
		// Create new window for this MAC
		window = &CorrelationWindow{
			MAC:       mac,
			TapObs:    make(map[string]*TapObservation),
			FirstSeen: now,
			LastSeen:  now,
		}
		sc.observations[mac] = window
	} else {
		// Check if window is stale and needs reset
		if now.Sub(window.LastSeen) > sc.windowDuration {
			window.TapObs = make(map[string]*TapObservation)
			window.FirstSeen = now
		}
		window.LastSeen = now
	}

	// Add or update the TAP observation
	window.TapObs[tapID] = &TapObservation{
		TapID:     tapID,
		Timestamp: now,
		RSSI:      rssi,
		Channel:   channel,
	}

	// Count active TAPs within the window duration
	activeTaps := sc.countActiveTapsLocked(window, now)

	// Build result with extended fields
	result := &CorrelationResult{
		TapsCount:         activeTaps,
		CombinedConfidence: baseConfidence,
		Reason:            PromotionNone,
		ConfidenceLevel:   "low",
		SingleTapEligible: false,
	}

	// Populate candidate-based fields if available
	if candidate != nil {
		result.Observations = candidate.Observations
		result.TimeSpanSec = candidate.LastSeen.Sub(candidate.FirstSeen).Seconds()
		result.MobilityScore = candidate.MobilityScore
		result.IsLikelyMobile = candidate.IsLikelyMobile
	}

	// === PATHWAY 1: Multi-TAP Correlation (highest confidence) ===
	if activeTaps >= sc.minTapsRequired {
		result.Confirmed = true
		result.Reason = PromotionMultiTap
		result.ConfidenceLevel = "high"
		// Boost confidence: base + (activeTaps - 1) * boost, capped at 0.95
		result.CombinedConfidence = baseConfidence + float32(activeTaps-1)*sc.confidenceBoost
		if result.CombinedConfidence > 0.95 {
			result.CombinedConfidence = 0.95
		}
		return result
	}

	// === SINGLE-TAP MODE PATHWAYS ===
	// Only evaluate if single-TAP mode is enabled and we have candidate data
	if !sc.singleTapMode || candidate == nil {
		return result
	}

	// Check if candidate meets single-TAP promotion thresholds
	minObs := cfg.SingleTapMinObservations
	minTimeSpanSec := float64(cfg.SingleTapMinTimeSpanSec)
	minMobility := cfg.SingleTapMinMobility

	// Apply defaults if config values are zero
	if minObs <= 0 {
		minObs = 5
	}
	if minTimeSpanSec <= 0 {
		minTimeSpanSec = 30
	}
	if minMobility <= 0 {
		minMobility = 0.6
	}

	timeSpan := result.TimeSpanSec
	observations := result.Observations

	// === PATHWAY 2: Manual Operator Confirmation ===
	if candidate.ManuallyConfirmed {
		result.Confirmed = true
		result.Reason = PromotionOperatorConfirmed
		result.ConfidenceLevel = "high"
		result.CombinedConfidence = 0.85
		return result
	}

	// === PATHWAY 3: High Mobility Score (single-TAP capable) ===
	// If the target shows clear mobile behavior (RSSI variance, channel changes)
	if candidate.IsLikelyMobile && candidate.MobilityScore >= minMobility {
		// Additional gate: require at least some observations
		if observations >= 3 {
			result.Confirmed = true
			result.Reason = PromotionSingleTapMobility
			result.ConfidenceLevel = "medium"
			// Confidence based on mobility score
			result.CombinedConfidence = baseConfidence + float32(candidate.MobilityScore)*0.25
			if result.CombinedConfidence > 0.80 {
				result.CombinedConfidence = 0.80
			}
			return result
		}
	}

	// === PATHWAY 4: Observation-Based Promotion (single-TAP with time) ===
	// Multiple observations over extended time suggests a real persistent target
	if observations >= minObs && timeSpan >= minTimeSpanSec {
		// Require some mobility indication to avoid false positives from static interference
		if candidate.MobilityScore >= 0.3 || candidate.RSSIVariance >= 8 {
			result.Confirmed = true
			result.Reason = PromotionSingleTapObservation
			result.ConfidenceLevel = "medium"
			// Base confidence + observation bonus
			obsBonus := float32(observations) / 20.0 // 20 obs = +1.0
			if obsBonus > 0.3 {
				obsBonus = 0.3
			}
			result.CombinedConfidence = baseConfidence + obsBonus
			if result.CombinedConfidence > 0.75 {
				result.CombinedConfidence = 0.75
			}
			return result
		}
	}

	// === Not yet promotable, but track eligibility ===
	// Mark as eligible if getting close to thresholds (for dashboard display)
	if observations >= minObs/2 || (timeSpan >= minTimeSpanSec/2 && candidate.MobilityScore >= 0.3) {
		result.SingleTapEligible = true
	}

	return result
}

// countActiveTapsLocked counts TAPs with observations within the window duration.
// Must be called with mu held.
func (sc *SuspectCorrelator) countActiveTapsLocked(window *CorrelationWindow, now time.Time) int {
	count := 0
	for _, obs := range window.TapObs {
		if now.Sub(obs.Timestamp) <= sc.windowDuration {
			count++
		}
	}
	return count
}

// Cleanup removes stale correlation windows older than maxAge.
// Returns the number of windows removed.
func (sc *SuspectCorrelator) Cleanup(maxAge time.Duration) int {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	now := time.Now()
	removed := 0

	for mac, window := range sc.observations {
		if now.Sub(window.LastSeen) > maxAge {
			delete(sc.observations, mac)
			removed++
		}
	}

	return removed
}

// GetPendingSuspects returns all correlation windows that have observations
// but have not yet reached the minTapsRequired threshold.
func (sc *SuspectCorrelator) GetPendingSuspects() []*CorrelationWindow {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	now := time.Now()
	var pending []*CorrelationWindow

	for _, window := range sc.observations {
		activeTaps := 0
		for _, obs := range window.TapObs {
			if now.Sub(obs.Timestamp) <= sc.windowDuration {
				activeTaps++
			}
		}

		// Only include windows that are still active but below threshold
		if activeTaps > 0 && activeTaps < sc.minTapsRequired {
			// Create a copy to avoid race conditions
			windowCopy := &CorrelationWindow{
				MAC:       window.MAC,
				TapObs:    make(map[string]*TapObservation),
				FirstSeen: window.FirstSeen,
				LastSeen:  window.LastSeen,
			}
			for tapID, obs := range window.TapObs {
				if now.Sub(obs.Timestamp) <= sc.windowDuration {
					windowCopy.TapObs[tapID] = &TapObservation{
						TapID:     obs.TapID,
						Timestamp: obs.Timestamp,
						RSSI:      obs.RSSI,
						Channel:   obs.Channel,
					}
				}
			}
			pending = append(pending, windowCopy)
		}
	}

	return pending
}
