// Package intel provides TDOA (Time Difference of Arrival) positioning
// for RF detections seen by multiple TAPs. When two TAPs detect the same
// signal burst, the time difference constrains the source to a hyperbola.
// Combined with RSSI range rings, this significantly tightens position estimates.
package intel

import (
	"math"
)

// TDOAMeasurement represents a time-difference measurement between two TAPs
type TDOAMeasurement struct {
	Tap1ID    string  // First TAP
	Tap1Lat   float64 // TAP 1 latitude
	Tap1Lon   float64 // TAP 1 longitude
	Tap2ID    string  // Second TAP
	Tap2Lat   float64 // TAP 2 latitude
	Tap2Lon   float64 // TAP 2 longitude
	DeltaNs   int64   // Time difference in nanoseconds (tap1 - tap2, positive = tap1 saw it first)
	TimingErr float64 // Estimated timing uncertainty in nanoseconds (from NTP sync quality)
}

// TDOAResult holds a TDOA-enhanced position estimate
type TDOAResult struct {
	Latitude    float64 // Estimated position
	Longitude   float64 // Estimated position
	ErrorM      float64 // Position uncertainty
	Confidence  float64 // 0-1 confidence
	Method      string  // "tdoa_rssi_fused" or "tdoa_only"
	HyperbolaID string  // Debug: which TAP pair produced this
}

// SpeedOfLight in meters per nanosecond
const SpeedOfLightMPerNs = 0.299792458

// TDOAToDistanceDiff converts a time difference (nanoseconds) to a distance difference (meters)
func TDOAToDistanceDiff(deltaNs int64) float64 {
	return float64(deltaNs) * SpeedOfLightMPerNs
}

// HyperbolaPoints generates points along a TDOA hyperbola in local ENU coordinates.
// The hyperbola is defined by the locus of points where the distance difference
// to two foci (TAPs) equals a constant (derived from time difference).
//
// Returns a slice of (lat, lon) points along the hyperbola, useful for:
// 1. Intersecting with RSSI range rings to refine position
// 2. Visualization on the dashboard
func HyperbolaPoints(m TDOAMeasurement, numPoints int) [][2]float64 {
	if numPoints < 2 {
		numPoints = 50
	}

	// Convert TAP positions to local ENU (meters) centered on midpoint
	midLat := (m.Tap1Lat + m.Tap2Lat) / 2
	midLon := (m.Tap1Lon + m.Tap2Lon) / 2

	t1e, t1n := llaToENU(m.Tap1Lat, m.Tap1Lon, midLat, midLon)
	t2e, t2n := llaToENU(m.Tap2Lat, m.Tap2Lon, midLat, midLon)

	// Distance between foci
	dx := t2e - t1e
	dy := t2n - t1n
	c := math.Sqrt(dx*dx+dy*dy) / 2 // Half distance between foci
	if c < 1 {
		return nil // TAPs too close for TDOA
	}

	// Semi-transverse axis: a = |delta_d| / 2
	deltaD := TDOAToDistanceDiff(m.DeltaNs)
	a := math.Abs(deltaD) / 2
	if a >= c {
		return nil // Invalid: distance difference exceeds baseline (bad timing)
	}

	// Semi-conjugate axis: b = sqrt(c² - a²)
	b := math.Sqrt(c*c - a*a)

	// Rotation angle of the baseline (TAP1 → TAP2)
	theta := math.Atan2(dy, dx)

	// Generate hyperbola points (parametric form)
	// x = a * cosh(t), y = b * sinh(t)
	// Only generate the branch closer to TAP that saw signal first
	points := make([][2]float64, 0, numPoints)
	tMax := 3.0 // Parametric range
	for i := 0; i < numPoints; i++ {
		t := -tMax + 2*tMax*float64(i)/float64(numPoints-1)

		// Hyperbola in local frame (aligned with baseline)
		hx := a * math.Cosh(t)
		hy := b * math.Sinh(t)

		// If TAP2 saw it first (deltaNs < 0), flip to other branch
		if m.DeltaNs < 0 {
			hx = -hx
		}

		// Rotate to geographic frame
		px := hx*math.Cos(theta) - hy*math.Sin(theta)
		py := hx*math.Sin(theta) + hy*math.Cos(theta)

		// Convert back to lat/lon (from midpoint-centered ENU)
		lat, lon := enuToLLA(px, py, midLat, midLon)
		points = append(points, [2]float64{lat, lon})
	}

	return points
}

// FuseRSSIAndTDOA combines RSSI range rings with TDOA hyperbola constraint
// to produce a more precise position estimate.
//
// Strategy: sample points along the TDOA hyperbola and find the point that
// best satisfies the RSSI range ring distances (minimum residual).
// This is simpler and more robust than full nonlinear optimization.
func FuseRSSIAndTDOA(
	tdoa TDOAMeasurement,
	rings []TapDistance, // RSSI range rings from trilateration.go
) *TDOAResult {
	// Generate candidate points along hyperbola
	candidates := HyperbolaPoints(tdoa, 200)
	if len(candidates) == 0 || len(rings) == 0 {
		return nil
	}

	bestResidual := math.MaxFloat64
	var bestLat, bestLon float64

	for _, pt := range candidates {
		lat, lon := pt[0], pt[1]

		// Compute total residual: sum of squared (measured_dist - predicted_dist) / uncertainty²
		residual := 0.0
		for _, ring := range rings {
			predicted := HaversineDistance(ring.Latitude, ring.Longitude, lat, lon)
			diff := predicted - ring.DistanceM
			unc := ring.UncertaintyM
			if unc < 50 {
				unc = 50
			}
			residual += (diff * diff) / (unc * unc)
		}

		if residual < bestResidual {
			bestResidual = residual
			bestLat = lat
			bestLon = lon
		}
	}

	if bestLat == 0 && bestLon == 0 {
		return nil
	}

	// Estimate error from timing uncertainty
	// At NTP accuracy (~1ms), timing error = 300km — too large
	// At GPS-disciplined (~1μs), timing error = 300m — useful
	// At PTP (~100ns), timing error = 30m — excellent
	timingErrorM := float64(tdoa.TimingErr) * SpeedOfLightMPerNs
	if timingErrorM < 50 {
		timingErrorM = 50
	}

	// Confidence based on residual quality and timing precision
	confidence := 0.5
	nRings := float64(len(rings))
	if nRings > 0 {
		normalizedResidual := bestResidual / nRings
		if normalizedResidual < 1 {
			confidence = 0.7
		} else if normalizedResidual < 4 {
			confidence = 0.5
		} else {
			confidence = 0.3
		}
	}
	// Bonus for good timing
	if tdoa.TimingErr < 1000 { // < 1μs
		confidence = math.Min(confidence+0.15, 0.85)
	}

	return &TDOAResult{
		Latitude:    bestLat,
		Longitude:   bestLon,
		ErrorM:      timingErrorM,
		Confidence:  confidence,
		Method:      "tdoa_rssi_fused",
		HyperbolaID: tdoa.Tap1ID + ":" + tdoa.Tap2ID,
	}
}

// MatchBurstTimestamps checks if two detections from different TAPs
// represent the same RF burst, suitable for TDOA computation.
// Returns true if the timestamps are close enough (within maxDeltaMs).
func MatchBurstTimestamps(ts1Ns, ts2Ns int64, maxDeltaMs float64) bool {
	deltaNs := ts1Ns - ts2Ns
	if deltaNs < 0 {
		deltaNs = -deltaNs
	}
	maxDeltaNs := int64(maxDeltaMs * 1e6)
	return deltaNs < maxDeltaNs
}
