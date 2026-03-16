package intel

import (
	"math"
	"sync"
	"time"
)

// RSSI Tracker Constants
const (
	// Approach/departure thresholds with hysteresis
	RSSITrendWindow    = 10   // Number of samples for trend analysis
	ApproachThreshold  = 0.5  // dBm/sample rise rate to ENTER approaching state
	DepartThreshold    = -0.5 // dBm/sample fall rate to ENTER departing state
	ApproachRelease    = 0.15 // must drop below this to leave approaching
	DepartRelease      = -0.15 // must rise above this to leave departing

	// Stale data cleanup
	DefaultHistorySize = 50
	DefaultMaxAge      = time.Hour

	// Mobility detection constants
	MobilityWindowDuration = 30 * time.Second // Window for mobility analysis
	MobilityThreshold      = 0.5              // Score threshold for IsLikelyMobile
)

// MovementState represents drone movement direction
type MovementState string

const (
	MovementApproaching MovementState = "approaching"
	MovementDeparting   MovementState = "departing"
	MovementStable      MovementState = "stable"
	MovementUnknown     MovementState = "unknown"
)

// RSSISample represents a single RSSI measurement
type RSSISample struct {
	Time time.Time
	RSSI float64
}

// DroneRSSI holds RSSI tracking data for a single drone
type DroneRSSI struct {
	Identifier      string
	Samples         []RSSISample
	Movement        MovementState
	MaxHistorySize  int
	MobilityProfile *MobilityProfile // Cached mobility profile, updated on Track()
}

// RSSIAnalysis contains the results of RSSI analysis
type RSSIAnalysis struct {
	RSSI           float64       `json:"rssi"`
	RSSIAvg        float64       `json:"rssi_avg"`
	RSSIMin        float64       `json:"rssi_min"`
	RSSIMax        float64       `json:"rssi_max"`
	DistanceEstM   float64       `json:"distance_est_m"`
	Trend          float64       `json:"trend"` // dBm per sample
	Movement       MovementState `json:"movement"`
	SampleCount    int           `json:"sample_count"`
}

// MobilityProfile contains RSSI-based mobility detection metrics
// Key insight: Drones MOVE so their RSSI varies 10-30dB over 30 seconds
// APs and fixed devices are stationary so RSSI varies <4dB
type MobilityProfile struct {
	RSSIVariance    float64       `json:"rssi_variance"`     // Variance of RSSI samples
	RSSIStdDev      float64       `json:"rssi_stddev"`       // Standard deviation
	RSSIRange       float64       `json:"rssi_range"`        // Max - Min in window
	RSSIJitter      float64       `json:"rssi_jitter"`       // Avg sample-to-sample delta
	DetrendedStdDev float64       `json:"detrended_stddev"`  // StdDev around trend line
	TrendSlope      float64       `json:"trend_slope"`       // dBm per sample slope
	MobilityScore   float64       `json:"mobility_score"`    // 0.0 stationary to 1.0 moving
	IsLikelyMobile  bool          `json:"is_likely_mobile"`  // Score > threshold
	SampleCount     int           `json:"sample_count"`      // Samples in analysis window
	WindowDuration  time.Duration `json:"window_duration"`   // Actual time span analyzed
}

// RSSITracker tracks RSSI per drone and detects approach/departure
type RSSITracker struct {
	mu          sync.RWMutex
	drones      map[string]*DroneRSSI
	historySize int

	// Stats
	stats struct {
		Samples              int64
		ApproachEvents       int64
		DepartEvents         int64
		MobilityDetections   int64 // Count of IsLikelyMobile=true detections
	}
}

// NewRSSITracker creates a new RSSI tracker
func NewRSSITracker(historySize int) *RSSITracker {
	if historySize <= 0 {
		historySize = DefaultHistorySize
	}
	return &RSSITracker{
		drones:      make(map[string]*DroneRSSI),
		historySize: historySize,
	}
}

// Track processes an RSSI reading for a drone
func (t *RSSITracker) Track(identifier string, rssi float64, model string) RSSIAnalysis {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()

	// Get or create drone record
	drone, exists := t.drones[identifier]
	if !exists {
		drone = &DroneRSSI{
			Identifier:     identifier,
			Samples:        make([]RSSISample, 0, t.historySize),
			Movement:       MovementStable,
			MaxHistorySize: t.historySize,
		}
		t.drones[identifier] = drone
	}

	// Add sample
	drone.Samples = append(drone.Samples, RSSISample{Time: now, RSSI: rssi})

	// Trim to max size
	if len(drone.Samples) > drone.MaxHistorySize {
		drone.Samples = drone.Samples[len(drone.Samples)-drone.MaxHistorySize:]
	}

	t.stats.Samples++

	// Compute metrics
	samples := make([]float64, len(drone.Samples))
	for i, s := range drone.Samples {
		samples[i] = s.RSSI
	}

	rssiAvg := mean(samples)
	rssiMin, rssiMax := minMax(samples)

	// Compute trend on last N samples
	trendSamples := samples
	if len(trendSamples) > RSSITrendWindow {
		trendSamples = trendSamples[len(trendSamples)-RSSITrendWindow:]
	}
	trend := computeRSSITrend(trendSamples)

	// Estimate distance
	distEst := EstimateDistanceFromRSSI(rssi, model, DefaultPathLossN)

	// Detect movement state change with hysteresis
	prevMovement := drone.Movement
	var movement MovementState

	switch prevMovement {
	case MovementApproaching:
		// Stay approaching until trend drops below release band
		if trend < ApproachRelease {
			if trend < DepartThreshold {
				movement = MovementDeparting
				t.stats.DepartEvents++
			} else {
				movement = MovementStable
			}
		} else {
			movement = MovementApproaching
		}

	case MovementDeparting:
		// Stay departing until trend rises above release band
		if trend > DepartRelease {
			if trend > ApproachThreshold {
				movement = MovementApproaching
				t.stats.ApproachEvents++
			} else {
				movement = MovementStable
			}
		} else {
			movement = MovementDeparting
		}

	default: // Stable or unknown
		// From stable: need full threshold to transition
		if trend > ApproachThreshold {
			movement = MovementApproaching
			t.stats.ApproachEvents++
		} else if trend < DepartThreshold {
			movement = MovementDeparting
			t.stats.DepartEvents++
		} else {
			movement = MovementStable
		}
	}

	drone.Movement = movement

	// Compute mobility profile on each update
	mobilityProfile := ComputeMobilityProfile(drone.Samples, MobilityWindowDuration)
	drone.MobilityProfile = &mobilityProfile

	// Track mobility detections
	if mobilityProfile.IsLikelyMobile {
		t.stats.MobilityDetections++
	}

	return RSSIAnalysis{
		RSSI:         rssi,
		RSSIAvg:      math.Round(rssiAvg*10) / 10,
		RSSIMin:      rssiMin,
		RSSIMax:      rssiMax,
		DistanceEstM: distEst,
		Trend:        math.Round(trend*1000) / 1000,
		Movement:     movement,
		SampleCount:  len(drone.Samples),
	}
}

// GetDroneRSSI returns current RSSI info for a specific drone
func (t *RSSITracker) GetDroneRSSI(identifier string) *RSSIAnalysis {
	t.mu.RLock()
	defer t.mu.RUnlock()

	drone, exists := t.drones[identifier]
	if !exists || len(drone.Samples) == 0 {
		return nil
	}

	samples := make([]float64, len(drone.Samples))
	for i, s := range drone.Samples {
		samples[i] = s.RSSI
	}

	latest := samples[len(samples)-1]
	rssiAvg := mean(samples)
	rssiMin, rssiMax := minMax(samples)

	trendSamples := samples
	if len(trendSamples) > RSSITrendWindow {
		trendSamples = trendSamples[len(trendSamples)-RSSITrendWindow:]
	}
	trend := computeRSSITrend(trendSamples)

	distEst := EstimateDistanceFromRSSI(latest, "", DefaultPathLossN)

	return &RSSIAnalysis{
		RSSI:         latest,
		RSSIAvg:      math.Round(rssiAvg*10) / 10,
		RSSIMin:      rssiMin,
		RSSIMax:      rssiMax,
		DistanceEstM: distEst,
		Trend:        math.Round(trend*1000) / 1000,
		Movement:     drone.Movement,
		SampleCount:  len(drone.Samples),
	}
}

// ClearStale removes old drone RSSI history to prevent memory growth
func (t *RSSITracker) ClearStale(maxAge time.Duration, maxTracked int) int {
	if maxAge == 0 {
		maxAge = DefaultMaxAge
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	staleCount := 0

	// Remove stale entries
	for id, drone := range t.drones {
		if len(drone.Samples) == 0 {
			delete(t.drones, id)
			staleCount++
			continue
		}
		lastSample := drone.Samples[len(drone.Samples)-1]
		if now.Sub(lastSample.Time) > maxAge {
			delete(t.drones, id)
			staleCount++
		}
	}

	// Hard cap: evict oldest if still over limit
	if maxTracked > 0 && len(t.drones) > maxTracked {
		// Find oldest entries
		type droneAge struct {
			id   string
			time time.Time
		}
		ages := make([]droneAge, 0, len(t.drones))
		for id, drone := range t.drones {
			if len(drone.Samples) > 0 {
				ages = append(ages, droneAge{id, drone.Samples[len(drone.Samples)-1].Time})
			}
		}
		// Simple bubble sort for oldest entries (acceptable for small N)
		for i := 0; i < len(ages)-1; i++ {
			for j := i + 1; j < len(ages); j++ {
				if ages[j].time.Before(ages[i].time) {
					ages[i], ages[j] = ages[j], ages[i]
				}
			}
		}
		// Remove oldest
		evictCount := len(t.drones) - maxTracked
		for i := 0; i < evictCount && i < len(ages); i++ {
			delete(t.drones, ages[i].id)
			staleCount++
		}
	}

	return staleCount
}

// Stats returns tracker statistics
func (t *RSSITracker) Stats() map[string]interface{} {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Count currently mobile transmitters
	mobileCount := 0
	for _, drone := range t.drones {
		if drone.MobilityProfile != nil && drone.MobilityProfile.IsLikelyMobile {
			mobileCount++
		}
	}

	return map[string]interface{}{
		"rssi_samples":        t.stats.Samples,
		"approach_events":     t.stats.ApproachEvents,
		"depart_events":       t.stats.DepartEvents,
		"tracked_drones":      len(t.drones),
		"mobility_detections": t.stats.MobilityDetections,
		"currently_mobile":    mobileCount,
	}
}

// computeRSSITrend calculates linear regression slope on RSSI samples
// Positive slope = signal getting stronger (approaching)
// Negative slope = signal getting weaker (departing)
func computeRSSITrend(samples []float64) float64 {
	n := len(samples)
	if n < 3 {
		return 0.0
	}

	// Simple least-squares slope
	xMean := float64(n-1) / 2.0
	yMean := mean(samples)

	numerator := 0.0
	denominator := 0.0
	for i, y := range samples {
		dx := float64(i) - xMean
		numerator += dx * (y - yMean)
		denominator += dx * dx
	}

	if denominator == 0 {
		return 0.0
	}

	return numerator / denominator
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func minMax(values []float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}
	min, max := values[0], values[0]
	for _, v := range values[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	return min, max
}

// ComputeMobilityProfile analyzes RSSI samples within a time window to detect mobility
// Drones move, causing RSSI to vary 10-30dB over 30 seconds
// Stationary devices (APs, fixed equipment) have RSSI variance <4dB
func ComputeMobilityProfile(samples []RSSISample, windowDuration time.Duration) MobilityProfile {
	if windowDuration == 0 {
		windowDuration = MobilityWindowDuration
	}

	profile := MobilityProfile{
		WindowDuration: windowDuration,
	}

	if len(samples) < 3 {
		// Not enough samples for meaningful analysis
		return profile
	}

	// Filter samples to those within the window
	now := time.Now()
	cutoff := now.Add(-windowDuration)

	var windowSamples []RSSISample
	for _, s := range samples {
		if s.Time.After(cutoff) {
			windowSamples = append(windowSamples, s)
		}
	}

	if len(windowSamples) < 3 {
		// Not enough samples in window
		profile.SampleCount = len(windowSamples)
		return profile
	}

	profile.SampleCount = len(windowSamples)

	// Calculate actual window duration from first to last sample
	if len(windowSamples) >= 2 {
		profile.WindowDuration = windowSamples[len(windowSamples)-1].Time.Sub(windowSamples[0].Time)
	}

	// Extract RSSI values
	rssiValues := make([]float64, len(windowSamples))
	for i, s := range windowSamples {
		rssiValues[i] = s.RSSI
	}

	// Compute basic statistics
	rssiMean := mean(rssiValues)
	rssiMin, rssiMax := minMax(rssiValues)
	profile.RSSIRange = rssiMax - rssiMin

	// Compute variance and standard deviation
	sumSqDiff := 0.0
	for _, v := range rssiValues {
		diff := v - rssiMean
		sumSqDiff += diff * diff
	}
	profile.RSSIVariance = sumSqDiff / float64(len(rssiValues))
	profile.RSSIStdDev = math.Sqrt(profile.RSSIVariance)

	// Compute jitter (average absolute sample-to-sample delta)
	if len(rssiValues) >= 2 {
		jitterSum := 0.0
		for i := 1; i < len(rssiValues); i++ {
			jitterSum += math.Abs(rssiValues[i] - rssiValues[i-1])
		}
		profile.RSSIJitter = jitterSum / float64(len(rssiValues)-1)
	}

	// Compute trend (linear regression slope)
	profile.TrendSlope = computeRSSITrend(rssiValues)

	// Compute detrended standard deviation (variance around trend line)
	// This isolates movement-induced variance from gradual approach/departure
	profile.DetrendedStdDev = computeDetrendedStdDev(rssiValues, profile.TrendSlope, rssiMean)

	// Compute mobility score based on multiple factors
	profile.MobilityScore = computeMobilityScore(profile)
	profile.IsLikelyMobile = profile.MobilityScore > MobilityThreshold

	return profile
}

// computeDetrendedStdDev calculates standard deviation of residuals after removing linear trend
func computeDetrendedStdDev(samples []float64, slope float64, yMean float64) float64 {
	if len(samples) < 3 {
		return 0.0
	}

	n := len(samples)
	xMean := float64(n-1) / 2.0

	// y-intercept: yMean = slope * xMean + intercept
	intercept := yMean - slope*xMean

	// Compute residuals (actual - predicted)
	sumSqResidual := 0.0
	for i, y := range samples {
		predicted := slope*float64(i) + intercept
		residual := y - predicted
		sumSqResidual += residual * residual
	}

	variance := sumSqResidual / float64(n)
	return math.Sqrt(variance)
}

// computeMobilityScore combines multiple RSSI metrics into a 0.0-1.0 mobility score
// Scoring based on empirical observations:
// - Drones in flight: range 10-30dB, stddev 4-10dB, visible trend
// - Fixed APs/devices: range <4dB, stddev <2dB, no trend
func computeMobilityScore(profile MobilityProfile) float64 {
	score := 0.0

	// RSSI Range scoring
	// >15dB: +0.4 (strong indication of movement)
	// >10dB: +0.3 (likely moving)
	// >6dB:  +0.15 (possibly moving)
	// <3dB:  -0.1 (stationary indicator)
	switch {
	case profile.RSSIRange > 15.0:
		score += 0.4
	case profile.RSSIRange > 10.0:
		score += 0.3
	case profile.RSSIRange > 6.0:
		score += 0.15
	case profile.RSSIRange < 3.0:
		score -= 0.1
	}

	// Trend magnitude scoring (absolute value - either approaching or departing)
	// >0.5 dB/sample: +0.3 (significant movement toward/away)
	// >0.3 dB/sample: +0.2
	// >0.15 dB/sample: +0.1
	absTrend := math.Abs(profile.TrendSlope)
	switch {
	case absTrend > 0.5:
		score += 0.3
	case absTrend > 0.3:
		score += 0.2
	case absTrend > 0.15:
		score += 0.1
	}

	// Standard deviation scoring
	// >6dB:   +0.3 (high variance = definitely moving)
	// >4dB:   +0.2 (moderate variance = likely moving)
	// >2.5dB: +0.1 (some variance)
	// <1.5dB: -0.15 (low variance = stationary indicator)
	switch {
	case profile.RSSIStdDev > 6.0:
		score += 0.3
	case profile.RSSIStdDev > 4.0:
		score += 0.2
	case profile.RSSIStdDev > 2.5:
		score += 0.1
	case profile.RSSIStdDev < 1.5:
		score -= 0.15
	}

	// Bonus for high jitter (rapid RSSI changes indicate movement through environment)
	if profile.RSSIJitter > 3.0 {
		score += 0.1
	}

	// Bonus for high detrended stddev (movement perpendicular to approach/depart line)
	if profile.DetrendedStdDev > 3.0 {
		score += 0.1
	}

	// Clamp to 0.0 - 1.0
	if score < 0.0 {
		score = 0.0
	}
	if score > 1.0 {
		score = 1.0
	}

	return math.Round(score*100) / 100 // Round to 2 decimal places
}

// GetMobilityProfile returns the cached mobility profile for a drone
func (t *RSSITracker) GetMobilityProfile(identifier string) *MobilityProfile {
	t.mu.RLock()
	defer t.mu.RUnlock()

	drone, exists := t.drones[identifier]
	if !exists {
		return nil
	}

	return drone.MobilityProfile
}

// GetAllMobileTransmitters returns all tracked transmitters with IsLikelyMobile=true
// This is the key function for catching unknown drones on 2.4GHz
func (t *RSSITracker) GetAllMobileTransmitters() map[string]*MobilityProfile {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make(map[string]*MobilityProfile)
	for id, drone := range t.drones {
		if drone.MobilityProfile != nil && drone.MobilityProfile.IsLikelyMobile {
			// Return a copy
			profile := *drone.MobilityProfile
			result[id] = &profile
		}
	}
	return result
}
