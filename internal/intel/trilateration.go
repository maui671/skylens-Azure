// Package intel provides intelligence features for drone detection
// including multi-tap trilateration for position estimation.
package intel

import (
	"errors"
	"math"
)

// Earth radius in meters for coordinate conversions
const EarthRadiusM = 6371000.0

// Trilateration errors
var (
	ErrInsufficientTaps    = errors.New("insufficient taps for trilateration (need >= 3)")
	ErrDegenerateGeometry  = errors.New("degenerate tap geometry (collinear or coincident)")
	ErrNoConvergence       = errors.New("trilateration did not converge")
	ErrInvalidInput        = errors.New("invalid input data")
)

// TapDistance represents a single tap's distance measurement to a target
type TapDistance struct {
	TapID        string  `json:"tap_id"`
	Latitude     float64 `json:"latitude"`      // Tap latitude in degrees
	Longitude    float64 `json:"longitude"`     // Tap longitude in degrees
	Altitude     float64 `json:"altitude"`      // Tap altitude in meters (optional)
	DistanceM    float64 `json:"distance_m"`    // Estimated distance to target in meters
	UncertaintyM float64 `json:"uncertainty_m"` // Distance uncertainty (1-sigma) in meters
	RSSI         float64 `json:"rssi"`          // Original RSSI value (for weighting)
	Confidence   float64 `json:"confidence"`    // Measurement confidence 0-1
}

// TrilaterationResult holds the computed position estimate
type TrilaterationResult struct {
	Latitude   float64 `json:"latitude"`    // Estimated latitude in degrees
	Longitude  float64 `json:"longitude"`   // Estimated longitude in degrees
	Altitude   float64 `json:"altitude"`    // Estimated altitude in meters (if 3D)
	ErrorM     float64 `json:"error_m"`     // Position uncertainty radius in meters
	Confidence float64 `json:"confidence"`  // Overall confidence 0-1
	TapsUsed   int     `json:"taps_used"`   // Number of taps used in calculation
	Iterations int     `json:"iterations"`  // Iterations to convergence
	Method     string  `json:"method"`      // Algorithm used

	// Uncertainty ellipse (2D)
	SemiMajorM float64 `json:"semi_major_m"` // Semi-major axis of error ellipse
	SemiMinorM float64 `json:"semi_minor_m"` // Semi-minor axis of error ellipse
	Orientation float64 `json:"orientation"` // Ellipse orientation in degrees from north
}

// TrilaterationConfig holds algorithm configuration
type TrilaterationConfig struct {
	MaxIterations      int     // Maximum iterations for iterative methods
	ConvergenceThresh  float64 // Convergence threshold in meters
	MinTaps            int     // Minimum taps required (default 3)
	UseAltitude        bool    // Enable 3D trilateration
	WeightByConfidence bool    // Weight measurements by confidence
	WeightByDistance   bool    // Weight inversely by distance (closer = higher weight)
	RobustOutlierReject bool   // Enable outlier rejection
	OutlierThresholdSigma float64 // Outlier threshold in sigmas
}

// DefaultTrilaterationConfig returns sensible defaults
func DefaultTrilaterationConfig() TrilaterationConfig {
	return TrilaterationConfig{
		MaxIterations:        50,
		ConvergenceThresh:    1.0, // 1 meter
		MinTaps:              3,
		UseAltitude:          false, // 2D by default
		WeightByConfidence:   true,
		WeightByDistance:     false, // Disabled: uncertainty already scales with distance
		RobustOutlierReject:  true,
		OutlierThresholdSigma: 2.5,
	}
}

// Trilaterate computes position from multiple tap distance measurements
// Uses weighted least squares with iterative refinement (Levenberg-Marquardt damped)
func Trilaterate(taps []TapDistance, cfg TrilaterationConfig) (*TrilaterationResult, error) {
	if len(taps) < cfg.MinTaps {
		return nil, ErrInsufficientTaps
	}

	// Validate inputs
	validTaps := make([]TapDistance, 0, len(taps))
	for _, t := range taps {
		if t.DistanceM > 0 && t.UncertaintyM > 0 {
			validTaps = append(validTaps, t)
		}
	}

	if len(validTaps) < cfg.MinTaps {
		return nil, ErrInvalidInput
	}

	// Check for degenerate geometry (collinear taps)
	if len(validTaps) >= 3 && isCollinear(validTaps) {
		return nil, ErrDegenerateGeometry
	}

	// Solve and optionally reject outliers
	result, err := trilaterateSolve(validTaps, cfg)
	if err != nil {
		return nil, err
	}

	// Outlier rejection: re-solve without bad taps
	if cfg.RobustOutlierReject && len(validTaps) > cfg.MinTaps {
		cleaned := rejectOutliers(validTaps, result, cfg)
		if len(cleaned) >= cfg.MinTaps && len(cleaned) < len(validTaps) {
			result2, err2 := trilaterateSolve(cleaned, cfg)
			if err2 == nil {
				result = result2
			}
		}
	}

	return result, nil
}

// rejectOutliers removes taps whose distance residual exceeds threshold * sigma
func rejectOutliers(taps []TapDistance, result *TrilaterationResult, cfg TrilaterationConfig) []TapDistance {
	refLat, refLon := taps[0].Latitude, taps[0].Longitude

	// Compute residuals at the solved position
	estX, estY := llaToENU(result.Latitude, result.Longitude, refLat, refLon)
	residuals := make([]float64, len(taps))
	var residSum float64
	for i, t := range taps {
		tx, ty := llaToENU(t.Latitude, t.Longitude, refLat, refLon)
		dx := estX - tx
		dy := estY - ty
		predDist := math.Sqrt(dx*dx + dy*dy)
		residuals[i] = math.Abs(t.DistanceM - predDist)
		residSum += residuals[i] * residuals[i]
	}

	// DOF = N - 2 (2D solve has 2 estimated parameters)
	dof := float64(len(taps)) - 2.0
	if dof < 1.0 {
		dof = 1.0
	}
	sigma := math.Sqrt(residSum / dof)
	if sigma < 10 {
		sigma = 10 // Floor to prevent over-aggressive rejection
	}

	threshold := cfg.OutlierThresholdSigma * sigma
	kept := make([]TapDistance, 0, len(taps))
	for i, t := range taps {
		if residuals[i] <= threshold {
			kept = append(kept, t)
		}
	}
	return kept
}

// trilaterateSolve is the core WLS solver with Levenberg-Marquardt damping
func trilaterateSolve(validTaps []TapDistance, cfg TrilaterationConfig) (*TrilaterationResult, error) {
	// Convert to local ENU coordinates centered on first tap
	refLat := validTaps[0].Latitude
	refLon := validTaps[0].Longitude

	// Build measurement vectors
	n := len(validTaps)
	tapX := make([]float64, n)
	tapY := make([]float64, n)
	distances := make([]float64, n)
	weights := make([]float64, n)

	for i, t := range validTaps {
		x, y := llaToENU(t.Latitude, t.Longitude, refLat, refLon)
		tapX[i] = x
		tapY[i] = y
		distances[i] = t.DistanceM
		weights[i] = calculateWeight(t, cfg)
	}

	// Initial estimate: weighted centroid biased by distances
	estX, estY := weightedCentroidEstimate(tapX, tapY, distances, weights)

	// Iterative weighted least squares with step damping
	var iterations int
	for iterations = 0; iterations < cfg.MaxIterations; iterations++ {
		// Compute residuals and Jacobian
		residuals := make([]float64, n)
		jacobianX := make([]float64, n)
		jacobianY := make([]float64, n)

		for i := range validTaps {
			dx := estX - tapX[i]
			dy := estY - tapY[i]
			predDist := math.Sqrt(dx*dx + dy*dy)

			if predDist < 1.0 {
				predDist = 1.0 // Avoid division by zero
			}

			residuals[i] = distances[i] - predDist
			// Jacobian sign: use +dx/d (negative of the true Jacobian ∂r/∂x = -dx/d)
			// so the solve delta = (J'WJ)^{-1} * J'Wr gives the correct GN step
			// (the sign flip absorbs the missing negation in the normal equation solve)
			jacobianX[i] = dx / predDist
			jacobianY[i] = dy / predDist
		}

		// Weighted normal equations: (J'WJ) * delta = J'W * residuals
		jwjXX, jwjXY, jwjYY := 0.0, 0.0, 0.0
		jwrX, jwrY := 0.0, 0.0

		for i := range validTaps {
			w := weights[i]
			jwjXX += w * jacobianX[i] * jacobianX[i]
			jwjXY += w * jacobianX[i] * jacobianY[i]
			jwjYY += w * jacobianY[i] * jacobianY[i]
			jwrX += w * jacobianX[i] * residuals[i]
			jwrY += w * jacobianY[i] * residuals[i]
		}

		// Solve 2x2 system
		det := jwjXX*jwjYY - jwjXY*jwjXY
		if math.Abs(det) < 1e-12 {
			return nil, ErrDegenerateGeometry
		}

		deltaX := (jwjYY*jwrX - jwjXY*jwrY) / det
		deltaY := (jwjXX*jwrY - jwjXY*jwrX) / det

		// Step damping: clamp step size to prevent divergence
		// Max step of 500m per iteration; if geometry needs bigger jumps,
		// it takes more iterations but doesn't oscillate
		const maxStepM = 500.0
		stepLen := math.Sqrt(deltaX*deltaX + deltaY*deltaY)
		if stepLen > maxStepM {
			scale := maxStepM / stepLen
			deltaX *= scale
			deltaY *= scale
		}

		// Update estimate
		estX += deltaX
		estY += deltaY

		// Check convergence (use original step length for convergence check)
		if stepLen < cfg.ConvergenceThresh {
			break
		}
	}

	if iterations >= cfg.MaxIterations {
		return nil, ErrNoConvergence
	}

	// Convert back to lat/lon
	estLat, estLon := enuToLLA(estX, estY, refLat, refLon)

	// Compute error statistics
	errorM, semiMajor, semiMinor, orientation := computeErrorEllipse(
		estX, estY, tapX, tapY, distances, weights,
	)

	// Compute GDOP for geometry quality assessment
	gdop := computeGDOP(estX, estY, tapX, tapY)

	// Compute overall confidence (GDOP penalizes poor geometry)
	confidence := computeOverallConfidence(validTaps, errorM)
	if gdop > 5 {
		confidence *= 0.7 // Poor geometry penalty
	} else if gdop > 3 {
		confidence *= 0.85
	}
	if confidence > 1 {
		confidence = 1
	}

	return &TrilaterationResult{
		Latitude:    estLat,
		Longitude:   estLon,
		Altitude:    0, // 2D only for now
		ErrorM:      math.Round(errorM*10) / 10,
		Confidence:  math.Round(confidence*100) / 100,
		TapsUsed:    len(validTaps),
		Iterations:  iterations + 1,
		Method:      "weighted_least_squares",
		SemiMajorM:  math.Round(semiMajor*10) / 10,
		SemiMinorM:  math.Round(semiMinor*10) / 10,
		Orientation: math.Round(orientation*10) / 10,
	}, nil
}

// TrilaterateSimple is a convenience function with default config
func TrilaterateSimple(taps []TapDistance) (*TrilaterationResult, error) {
	return Trilaterate(taps, DefaultTrilaterationConfig())
}

// calculateWeight computes measurement weight based on configuration
func calculateWeight(t TapDistance, cfg TrilaterationConfig) float64 {
	weight := 1.0

	// Weight by inverse variance (uncertainty)
	if t.UncertaintyM > 0 {
		weight *= 1.0 / (t.UncertaintyM * t.UncertaintyM)
	}

	// Additional weighting by confidence
	if cfg.WeightByConfidence && t.Confidence > 0 {
		weight *= t.Confidence
	}

	// Weight by inverse distance (closer measurements are more accurate)
	if cfg.WeightByDistance && t.DistanceM > 0 {
		// Use square root to moderate the effect
		weight *= 1.0 / math.Sqrt(t.DistanceM/1000.0+1.0)
	}

	return weight
}

// isCollinear checks if taps are approximately collinear
func isCollinear(taps []TapDistance) bool {
	if len(taps) < 3 {
		return true
	}

	// Use cross product to check collinearity
	// If all points are collinear, cross products will be near zero
	refLat := taps[0].Latitude
	refLon := taps[0].Longitude

	x0, y0 := llaToENU(taps[0].Latitude, taps[0].Longitude, refLat, refLon)
	x1, y1 := llaToENU(taps[1].Latitude, taps[1].Longitude, refLat, refLon)

	v1x, v1y := x1-x0, y1-y0
	v1len := math.Sqrt(v1x*v1x + v1y*v1y)
	if v1len < 1.0 { // Points too close
		return true
	}

	// Check remaining points
	maxCross := 0.0
	for i := 2; i < len(taps); i++ {
		xi, yi := llaToENU(taps[i].Latitude, taps[i].Longitude, refLat, refLon)
		v2x, v2y := xi-x0, yi-y0

		// Cross product magnitude
		cross := math.Abs(v1x*v2y - v1y*v2x)
		if cross > maxCross {
			maxCross = cross
		}
	}

	// If cross product is small relative to distances, points are collinear
	// Threshold: triangle area < 100 sq meters
	return maxCross < 200.0
}

// weightedCentroidEstimate computes initial position estimate biased by distance measurements.
// For each tap, places a candidate point at the measured distance along the tap-to-centroid
// direction, then takes the weighted average. This gives a much better starting point for
// the iterative solver than the raw centroid (which ignores distance data entirely).
func weightedCentroidEstimate(tapX, tapY, distances, weights []float64) (float64, float64) {
	n := len(tapX)

	// First compute unbiased weighted centroid
	totalWeight := 0.0
	sumX, sumY := 0.0, 0.0
	for i := 0; i < n; i++ {
		totalWeight += weights[i]
		sumX += weights[i] * tapX[i]
		sumY += weights[i] * tapY[i]
	}

	if totalWeight <= 0 {
		// Fallback: simple unweighted average (reset accumulators first)
		sumX, sumY = 0.0, 0.0
		fn := float64(n)
		for i := 0; i < n; i++ {
			sumX += tapX[i]
			sumY += tapY[i]
		}
		return sumX / fn, sumY / fn
	}

	cx := sumX / totalWeight
	cy := sumY / totalWeight

	// Bias centroid using distances: for each tap, project a point at measured
	// distance along the tap→centroid direction, then average those points.
	pullX, pullY := 0.0, 0.0
	pullW := 0.0
	for i := 0; i < n; i++ {
		dx := cx - tapX[i]
		dy := cy - tapY[i]
		centroidDist := math.Sqrt(dx*dx + dy*dy)
		if centroidDist < 1 {
			continue // tap is at centroid, skip
		}
		ratio := distances[i] / centroidDist
		pullX += weights[i] * (tapX[i] + dx*ratio)
		pullY += weights[i] * (tapY[i] + dy*ratio)
		pullW += weights[i]
	}

	if pullW > 0 {
		return pullX / pullW, pullY / pullW
	}
	return cx, cy
}

// computeErrorEllipse computes position uncertainty ellipse from actual covariance
// Returns: errorM (circular equivalent), semiMajor, semiMinor, orientation (degrees from north)
func computeErrorEllipse(estX, estY float64, tapX, tapY, distances, weights []float64) (float64, float64, float64, float64) {
	n := len(tapX)
	if n < 2 {
		return 100.0, 100.0, 100.0, 0.0 // Default large uncertainty
	}

	// Compute weighted residual variance
	sumWeightedResidSq := 0.0
	totalWeight := 0.0

	// Build Jacobian matrix components
	var hxx, hxy, hyy float64 // H'WH components

	for i := range tapX {
		dx := estX - tapX[i]
		dy := estY - tapY[i]
		predDist := math.Sqrt(dx*dx + dy*dy)
		if predDist < 1.0 {
			predDist = 1.0
		}

		resid := distances[i] - predDist
		sumWeightedResidSq += weights[i] * resid * resid
		totalWeight += weights[i]

		// Jacobian components (direction cosines)
		jx := dx / predDist
		jy := dy / predDist

		// Accumulate H'WH (weighted normal matrix)
		w := weights[i]
		hxx += w * jx * jx
		hxy += w * jx * jy
		hyy += w * jy * jy
	}

	// Estimate measurement variance with degrees-of-freedom correction
	// (N-2 for 2 estimated parameters; prevents 73% underestimate with 3 TAPs)
	// BUG FIX: was using totalWeight (sum of weights) instead of N (number of taps).
	// With heterogeneous weights, totalWeight can be orders of magnitude larger than N,
	// causing gross underestimation of the variance (and thus error ellipse).
	dof := float64(n) - 2.0
	if dof < 1.0 {
		dof = 1.0
	}
	sigmaSquared := sumWeightedResidSq / dof
	if sigmaSquared < 1.0 {
		sigmaSquared = 1.0 // Minimum variance floor
	}

	// Compute covariance matrix: C = sigma^2 * (H'WH)^-1
	// For 2x2: inverse of [[hxx, hxy], [hxy, hyy]]
	det := hxx*hyy - hxy*hxy
	if math.Abs(det) < 1e-10 {
		// Singular matrix - poor geometry
		return 500.0, 500.0, 500.0, 0.0
	}

	// Covariance matrix elements
	cxx := sigmaSquared * hyy / det
	cxy := -sigmaSquared * hxy / det
	cyy := sigmaSquared * hxx / det

	// Compute eigenvalues for error ellipse
	// lambda = 0.5 * (trace +/- sqrt(trace^2 - 4*det))
	trace := cxx + cyy
	covDet := cxx*cyy - cxy*cxy
	discriminant := trace*trace - 4*covDet

	var lambda1, lambda2 float64
	if discriminant < 0 {
		// Numerical issue - use circular
		lambda1 = trace / 2
		lambda2 = trace / 2
	} else {
		sqrtDisc := math.Sqrt(discriminant)
		lambda1 = (trace + sqrtDisc) / 2 // Larger eigenvalue
		lambda2 = (trace - sqrtDisc) / 2 // Smaller eigenvalue
	}

	// Ensure positive
	if lambda1 < 0 {
		lambda1 = 1.0
	}
	if lambda2 < 0 {
		lambda2 = 1.0
	}

	// Semi-axes are sqrt of eigenvalues (1-sigma)
	semiMajor := math.Sqrt(lambda1)
	semiMinor := math.Sqrt(lambda2)

	// Ensure major >= minor
	if semiMinor > semiMajor {
		semiMajor, semiMinor = semiMinor, semiMajor
	}

	// Compute orientation (angle of major axis from north/Y-axis)
	// Eigenvector for lambda1: direction of maximum uncertainty
	var orientation float64
	if math.Abs(cxy) < 1e-10 {
		// No off-diagonal - axes aligned with E/N
		if cxx > cyy {
			orientation = 90.0 // Major axis is East
		} else {
			orientation = 0.0 // Major axis is North
		}
	} else {
		// Angle from X-axis (East) to major axis
		theta := 0.5 * math.Atan2(2*cxy, cxx-cyy)
		// Convert to degrees from North (clockwise)
		orientation = 90.0 - theta*180.0/math.Pi
		// Normalize to 0-180
		for orientation < 0 {
			orientation += 180
		}
		for orientation >= 180 {
			orientation -= 180
		}
	}

	// Circular equivalent error (CEP-like)
	errorM := math.Sqrt(semiMajor*semiMajor + semiMinor*semiMinor)

	// Apply floors and ceilings
	if errorM < 10 {
		errorM = 10
	}
	if errorM > 5000 {
		errorM = 5000
	}
	if semiMajor < 10 {
		semiMajor = 10
	}
	if semiMajor > 5000 {
		semiMajor = 5000
	}
	if semiMinor < 5 {
		semiMinor = 5
	}
	if semiMinor > semiMajor {
		semiMinor = semiMajor
	}

	return errorM, semiMajor, semiMinor, orientation
}

// computeGDOP estimates geometric dilution of precision
func computeGDOP(estX, estY float64, tapX, tapY []float64) float64 {
	n := len(tapX)
	if n < 2 {
		return 5.0 // High GDOP for few taps
	}

	// Build geometry matrix H (direction cosines)
	// H = [-dx/d, -dy/d] for each tap
	// GDOP = sqrt(trace((H'H)^-1))

	hxx, hxy, hyy := 0.0, 0.0, 0.0

	for i := range tapX {
		dx := estX - tapX[i]
		dy := estY - tapY[i]
		d := math.Sqrt(dx*dx + dy*dy)
		if d < 1 {
			d = 1
		}

		hx := dx / d
		hy := dy / d

		hxx += hx * hx
		hxy += hx * hy
		hyy += hy * hy
	}

	// Compute (H'H)^-1 determinant
	det := hxx*hyy - hxy*hxy
	if math.Abs(det) < 1e-10 {
		return 10.0 // Poor geometry
	}

	// Trace of inverse
	traceInv := (hxx + hyy) / det
	if traceInv < 0 {
		return 5.0
	}

	gdop := math.Sqrt(traceInv)

	// Clamp to reasonable range
	if gdop < 1 {
		gdop = 1
	}
	if gdop > 20 {
		gdop = 20
	}

	return gdop
}

// computeOverallConfidence computes combined confidence from measurements and geometry
func computeOverallConfidence(taps []TapDistance, errorM float64) float64 {
	if len(taps) == 0 {
		return 0
	}

	// Average measurement confidence
	sumConf := 0.0
	for _, t := range taps {
		sumConf += t.Confidence
	}
	avgConf := sumConf / float64(len(taps))

	// Geometry confidence (based on error)
	geoConf := 1.0
	if errorM > 1000 {
		geoConf = 0.3
	} else if errorM > 500 {
		geoConf = 0.5
	} else if errorM > 200 {
		geoConf = 0.7
	} else if errorM > 100 {
		geoConf = 0.85
	}

	// Tap count bonus
	tapBonus := 0.0
	if len(taps) >= 4 {
		tapBonus = 0.1
	}
	if len(taps) >= 6 {
		tapBonus = 0.15
	}

	confidence := avgConf*0.5 + geoConf*0.5 + tapBonus
	if confidence > 1.0 {
		confidence = 1.0
	}

	return confidence
}

// llaToENU converts lat/lon to local East-North-Up coordinates
func llaToENU(lat, lon, refLat, refLon float64) (float64, float64) {
	// Convert to radians
	latRad := lat * math.Pi / 180.0
	lonRad := lon * math.Pi / 180.0
	refLatRad := refLat * math.Pi / 180.0
	refLonRad := refLon * math.Pi / 180.0

	// Difference
	dLat := latRad - refLatRad
	dLon := lonRad - refLonRad

	// Convert to meters (small angle approximation)
	// East = R * cos(lat) * dLon
	// North = R * dLat
	east := EarthRadiusM * math.Cos(refLatRad) * dLon
	north := EarthRadiusM * dLat

	return east, north
}

// enuToLLA converts local ENU coordinates back to lat/lon
func enuToLLA(east, north, refLat, refLon float64) (float64, float64) {
	refLatRad := refLat * math.Pi / 180.0

	// Convert meters to degrees
	dLat := north / EarthRadiusM
	dLon := east / (EarthRadiusM * math.Cos(refLatRad))

	// Convert to degrees and add to reference
	lat := refLat + dLat*180.0/math.Pi
	lon := refLon + dLon*180.0/math.Pi

	return lat, lon
}

// SingleTapRangeRing generates a range ring for visualization from single tap
func SingleTapRangeRing(tap TapDistance) RangeRing {
	minM := tap.DistanceM - tap.UncertaintyM
	if minM < 0 {
		minM = 0
	}
	return RangeRing{
		TapID:      tap.TapID,
		TapLat:     tap.Latitude,
		TapLon:     tap.Longitude,
		DistanceM:  tap.DistanceM,
		MinM:       minM,
		MaxM:       tap.DistanceM + tap.UncertaintyM,
		Confidence: tap.Confidence,
	}
}

// RangeRing represents a distance ring from a single tap for visualization
type RangeRing struct {
	TapID      string  `json:"tap_id"`
	TapLat     float64 `json:"tap_lat"`
	TapLon     float64 `json:"tap_lon"`
	DistanceM  float64 `json:"distance_m"`
	MinM       float64 `json:"min_m"`
	MaxM       float64 `json:"max_m"`
	Confidence float64 `json:"confidence"`
}
