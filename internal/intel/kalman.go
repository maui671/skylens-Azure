// Package intel provides intelligence features for drone detection
// including Kalman filtering for trajectory smoothing.
package intel

import (
	"math"
	"sync"
	"time"
)

// Kalman filter state vector: [north_m, east_m, v_north, v_east]
// north_m and east_m are offsets in meters from a reference point (first measurement).
// v_north and v_east are velocities in m/s.

const (
	// Process noise: acceleration standard deviation (m/s^2)
	// CWNA model — expects ~2 m/s^2 unmodeled acceleration (reasonable for drones)
	kalmanQAccel = 2.0
	// Minimum time between updates to prevent numerical issues
	kalmanMinDtSec = 0.01
	// Maximum time gap — reset filter if exceeded
	kalmanMaxDtSec = 60.0
	// Innovation gate threshold (Mahalanobis distance squared)
	// chi-square with 2 DOF at 99% = 9.21
	kalmanInnovationGate = 9.21
	// Maximum drones tracked before LRU eviction
	kalmanMaxTracked = 5000
)

// DroneKalmanFilter implements a 4-state constant-velocity Kalman filter
// for drone trajectory smoothing in local ENU coordinates.
// Thread-safe: all public methods acquire the internal mutex.
type DroneKalmanFilter struct {
	mu sync.Mutex
	// State vector [lat_m, lon_m, v_north, v_east]
	x [4]float64
	// Covariance matrix 4x4 (symmetric, stored as full matrix for clarity)
	P [4][4]float64
	// Reference point for local coordinate conversion
	refLat, refLon float64
	refSet         bool
	// Last update time
	lastUpdate time.Time
	initialized bool
}

// KalmanRegistry manages per-drone Kalman filters with LRU eviction.
type KalmanRegistry struct {
	mu      sync.Mutex
	filters map[string]*kalmanEntry
}

type kalmanEntry struct {
	filter     *DroneKalmanFilter
	lastAccess time.Time
}

// NewKalmanRegistry creates a new filter registry.
func NewKalmanRegistry() *KalmanRegistry {
	return &KalmanRegistry{
		filters: make(map[string]*kalmanEntry),
	}
}

// GetOrCreate returns the Kalman filter for a drone, creating one if needed.
func (kr *KalmanRegistry) GetOrCreate(droneID string) *DroneKalmanFilter {
	kr.mu.Lock()
	defer kr.mu.Unlock()

	entry, ok := kr.filters[droneID]
	if ok {
		entry.lastAccess = time.Now()
		return entry.filter
	}

	// Create new filter
	kf := &DroneKalmanFilter{}
	kr.filters[droneID] = &kalmanEntry{
		filter:     kf,
		lastAccess: time.Now(),
	}

	// LRU eviction if over limit
	if len(kr.filters) > kalmanMaxTracked {
		kr.evictOldest()
	}

	return kf
}

// Remove deletes a drone's filter (e.g., when drone goes LOST)
func (kr *KalmanRegistry) Remove(droneID string) {
	kr.mu.Lock()
	delete(kr.filters, droneID)
	kr.mu.Unlock()
}

// Count returns number of active filters
func (kr *KalmanRegistry) Count() int {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	return len(kr.filters)
}

func (kr *KalmanRegistry) evictOldest() {
	var oldestID string
	var oldestTime time.Time
	first := true
	for id, entry := range kr.filters {
		if first || entry.lastAccess.Before(oldestTime) {
			oldestID = id
			oldestTime = entry.lastAccess
			first = false
		}
	}
	if oldestID != "" {
		delete(kr.filters, oldestID)
	}
}

// KalmanMeasurement represents a position measurement to feed into the filter.
type KalmanMeasurement struct {
	Lat       float64 // degrees
	Lon       float64 // degrees
	ErrorM    float64 // measurement uncertainty in meters (1-sigma)
	Timestamp time.Time
	Source    string  // "gps", "trilaterated", "range_bearing"
}

// KalmanEstimate is the filtered position output.
type KalmanEstimate struct {
	Lat        float64   `json:"latitude"`
	Lon        float64   `json:"longitude"`
	VNorth     float64   `json:"v_north"`     // m/s
	VEast      float64   `json:"v_east"`      // m/s
	SpeedMps   float64   `json:"speed_mps"`   // ground speed m/s
	ErrorM     float64   `json:"error_m"`     // position uncertainty (sqrt of trace)
	Timestamp  time.Time `json:"timestamp"`
}

// Update processes a new measurement and returns the filtered estimate.
// Thread-safe: acquires per-filter mutex (concurrent trilateration goroutines
// for the same drone can call this simultaneously from different TAP detections).
func (kf *DroneKalmanFilter) Update(m KalmanMeasurement) *KalmanEstimate {
	kf.mu.Lock()
	defer kf.mu.Unlock()

	now := m.Timestamp
	if now.IsZero() {
		now = time.Now()
	}

	// Set reference point on first measurement
	if !kf.refSet {
		kf.refLat = m.Lat
		kf.refLon = m.Lon
		kf.refSet = true
	}

	// Convert measurement to local meters
	mX, mY := llaToENU(m.Lat, m.Lon, kf.refLat, kf.refLon)

	// Initialize filter on first measurement
	if !kf.initialized {
		kf.x[0] = mY // north (lat_m)
		kf.x[1] = mX // east (lon_m)
		kf.x[2] = 0  // v_north
		kf.x[3] = 0  // v_east

		// Initial covariance: high uncertainty in position, very high in velocity
		sigma := m.ErrorM
		if sigma < 50 {
			sigma = 50
		}
		kf.P[0][0] = sigma * sigma
		kf.P[1][1] = sigma * sigma
		kf.P[2][2] = 25.0 // 5 m/s uncertainty
		kf.P[3][3] = 25.0

		kf.lastUpdate = now
		kf.initialized = true
		return kf.getEstimate(now)
	}

	// Compute time delta
	dt := now.Sub(kf.lastUpdate).Seconds()
	if dt < kalmanMinDtSec {
		return kf.getEstimate(now) // Too fast, return current estimate
	}
	if dt > kalmanMaxDtSec {
		// Too long gap — reset filter with this measurement
		kf.initialized = false
		kf.refSet = false // Re-center reference point on new measurement
		return kf.Update(m)
	}

	// === PREDICT ===
	// State transition: constant velocity model
	// x_pred = F * x
	xPred := [4]float64{
		kf.x[0] + kf.x[2]*dt, // lat_m + v_north * dt
		kf.x[1] + kf.x[3]*dt, // lon_m + v_east * dt
		kf.x[2],               // v_north (constant)
		kf.x[3],               // v_east (constant)
	}

	// Process noise Q: Continuous White Noise Acceleration (CWNA) model
	// Q_axis = qa * [[dt^3/3, dt^2/2], [dt^2/2, dt]]
	// Guaranteed positive-semidefinite for all dt > 0
	qa := kalmanQAccel * kalmanQAccel
	qPos := qa * dt * dt * dt / 3.0
	qVel := qa * dt
	qPosVel := qa * dt * dt / 2.0

	// Predicted covariance: P_pred = F*P*F' + Q
	// F = [[1, 0, dt, 0], [0, 1, 0, dt], [0, 0, 1, 0], [0, 0, 0, 1]]
	var pPred [4][4]float64

	// F*P
	var fp [4][4]float64
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			fp[i][j] = kf.P[i][j]
		}
	}
	// Rows 0,1 get dt * rows 2,3 added
	for j := 0; j < 4; j++ {
		fp[0][j] += dt * kf.P[2][j]
		fp[1][j] += dt * kf.P[3][j]
	}

	// (F*P)*F'
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			pPred[i][j] = fp[i][j]
		}
	}
	// Columns 0,1 get dt * columns 2,3 added
	for i := 0; i < 4; i++ {
		pPred[i][0] += dt * fp[i][2]
		pPred[i][1] += dt * fp[i][3]
	}

	// Add process noise Q
	pPred[0][0] += qPos
	pPred[1][1] += qPos
	pPred[2][2] += qVel
	pPred[3][3] += qVel
	pPred[0][2] += qPosVel
	pPred[2][0] += qPosVel
	pPred[1][3] += qPosVel
	pPred[3][1] += qPosVel

	// === INNOVATION ===
	// Measurement model: H = [[1, 0, 0, 0], [0, 1, 0, 0]]
	// z = [mY, mX] (north, east in local meters)
	// innovation = z - H * x_pred
	innovN := mY - xPred[0]
	innovE := mX - xPred[1]

	// Measurement noise R: scale by source quality
	rBase := m.ErrorM * m.ErrorM
	if rBase < 100 { // Min 10m uncertainty
		rBase = 100
	}
	switch m.Source {
	case "gps":
		// GPS is most accurate — use reported error
	case "weighted_least_squares", "trilaterated":
		rBase *= 1.5 // Multi-TAP WLS — best non-GPS method
	case "circle_intersection":
		rBase *= 2.5 // 2-TAP intersection — moderate uncertainty
	case "range_bearing":
		rBase *= 4.0 // Single-tap is very uncertain
	default:
		rBase *= 2.0
	}

	// Innovation covariance: S = H*P_pred*H' + R
	s00 := pPred[0][0] + rBase // north-north
	s01 := pPred[0][1]         // north-east
	s10 := pPred[1][0]         // east-north
	s11 := pPred[1][1] + rBase // east-east

	// Innovation gating (Mahalanobis distance)
	sDet := s00*s11 - s01*s10
	if math.Abs(sDet) < 1e-10 {
		return kf.getEstimate(now) // Singular, skip update
	}
	sInv00 := s11 / sDet
	sInv01 := -s01 / sDet
	sInv10 := -s10 / sDet
	sInv11 := s00 / sDet

	mahal := innovN*innovN*sInv00 + innovN*innovE*(sInv01+sInv10) + innovE*innovE*sInv11
	if mahal > kalmanInnovationGate {
		// Outlier — skip this measurement but still apply prediction
		kf.x = xPred
		kf.P = pPred
		kf.lastUpdate = now
		return kf.getEstimate(now)
	}

	// === KALMAN GAIN ===
	// K = P_pred * H' * S^-1
	// K is 4x2
	var K [4][2]float64
	for i := 0; i < 4; i++ {
		// P_pred * H' is just columns 0,1 of P_pred
		ph0 := pPred[i][0]
		ph1 := pPred[i][1]
		K[i][0] = ph0*sInv00 + ph1*sInv10
		K[i][1] = ph0*sInv01 + ph1*sInv11
	}

	// === UPDATE ===
	// x = x_pred + K * innovation
	kf.x[0] = xPred[0] + K[0][0]*innovN + K[0][1]*innovE
	kf.x[1] = xPred[1] + K[1][0]*innovN + K[1][1]*innovE
	kf.x[2] = xPred[2] + K[2][0]*innovN + K[2][1]*innovE
	kf.x[3] = xPred[3] + K[3][0]*innovN + K[3][1]*innovE

	// P = P_pred - K*S*K' (symmetric update form)
	var ksk [4][4]float64
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			ksk[i][j] = K[i][0]*(s00*K[j][0]+s01*K[j][1]) +
				K[i][1]*(s10*K[j][0]+s11*K[j][1])
		}
	}
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			kf.P[i][j] = pPred[i][j] - ksk[i][j]
		}
	}

	// Ensure symmetry (numerical drift)
	for i := 0; i < 4; i++ {
		for j := i + 1; j < 4; j++ {
			avg := (kf.P[i][j] + kf.P[j][i]) / 2
			kf.P[i][j] = avg
			kf.P[j][i] = avg
		}
	}

	kf.lastUpdate = now
	return kf.getEstimate(now)
}

// Predict returns the predicted state extrapolated to time t using the
// constant-velocity model, without incorporating a new measurement.
// Useful for dead-reckoning or display extrapolation between updates.
func (kf *DroneKalmanFilter) Predict(t time.Time) *KalmanEstimate {
	kf.mu.Lock()
	defer kf.mu.Unlock()

	if !kf.initialized {
		return nil
	}

	dt := t.Sub(kf.lastUpdate).Seconds()
	if dt <= 0 {
		return kf.getEstimate(t)
	}
	if dt > kalmanMaxDtSec {
		dt = kalmanMaxDtSec // cap extrapolation
	}

	// Extrapolate state: x_pred = F * x
	predN := kf.x[0] + kf.x[2]*dt
	predE := kf.x[1] + kf.x[3]*dt

	lat, lon := enuToLLA(predE, predN, kf.refLat, kf.refLon)

	// Extrapolate uncertainty: P_pred grows with dt
	qa := kalmanQAccel * kalmanQAccel
	errorM := math.Sqrt(kf.P[0][0] + kf.P[1][1] + 2*qa*dt*dt*dt/3.0)
	if errorM < 5 {
		errorM = 5
	}
	if errorM > 5000 {
		errorM = 5000
	}

	speed := math.Sqrt(kf.x[2]*kf.x[2] + kf.x[3]*kf.x[3])

	return &KalmanEstimate{
		Lat:       lat,
		Lon:       lon,
		VNorth:    math.Round(kf.x[2]*100) / 100,
		VEast:     math.Round(kf.x[3]*100) / 100,
		SpeedMps:  math.Round(speed*100) / 100,
		ErrorM:    math.Round(errorM*10) / 10,
		Timestamp: t,
	}
}

func (kf *DroneKalmanFilter) getEstimate(t time.Time) *KalmanEstimate {
	// Convert local meters back to lat/lon
	lat, lon := enuToLLA(kf.x[1], kf.x[0], kf.refLat, kf.refLon)

	// Position uncertainty: sqrt of position covariance trace
	errorM := math.Sqrt(kf.P[0][0] + kf.P[1][1])
	if errorM < 5 {
		errorM = 5
	}
	if errorM > 5000 {
		errorM = 5000
	}

	speed := math.Sqrt(kf.x[2]*kf.x[2] + kf.x[3]*kf.x[3])

	return &KalmanEstimate{
		Lat:       lat,
		Lon:       lon,
		VNorth:    math.Round(kf.x[2]*100) / 100,
		VEast:     math.Round(kf.x[3]*100) / 100,
		SpeedMps:  math.Round(speed*100) / 100,
		ErrorM:    math.Round(errorM*10) / 10,
		Timestamp: t,
	}
}
