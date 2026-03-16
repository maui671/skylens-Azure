package intel

import (
	"math"
	"sync"
	"testing"
	"time"
)

// === Helper ===

func newTestCalibrator() *LiveCalibrator {
	return NewLiveCalibrator(DefaultPathLossN)
}

// feedCalibrationPoints adds N calibration points for a model+tap
func feedCalibrationPoints(lc *LiveCalibrator, model, tapID string, n int, rssi, distance float64) {
	for i := 0; i < n; i++ {
		lc.RecordCalibrationPoint(model, tapID, rssi, distance)
	}
}

// === Tests: NewLiveCalibrator ===

func TestNewLiveCalibrator_DefaultPathLoss(t *testing.T) {
	lc := NewLiveCalibrator(0) // 0 = use default
	if lc.pathLossN != DefaultPathLossN {
		t.Errorf("expected default path loss %f, got %f", DefaultPathLossN, lc.pathLossN)
	}
}

func TestNewLiveCalibrator_CustomPathLoss(t *testing.T) {
	lc := NewLiveCalibrator(2.5)
	if lc.pathLossN != 2.5 {
		t.Errorf("expected path loss 2.5, got %f", lc.pathLossN)
	}
}

func TestNewLiveCalibrator_EmptyMaps(t *testing.T) {
	lc := newTestCalibrator()
	if len(lc.models) != 0 {
		t.Error("expected empty models map")
	}
	if len(lc.identities) != 0 {
		t.Error("expected empty identities map")
	}
}

// === Tests: RecordCalibrationPoint ===

func TestRecordCalibrationPoint_ValidInput(t *testing.T) {
	lc := newTestCalibrator()
	lc.RecordCalibrationPoint("Air 2S", "tap-1", -75.0, 1500.0)

	if len(lc.models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(lc.models))
	}
	cal := lc.models["Air 2S"]
	if cal == nil {
		t.Fatal("expected Air 2S model calibration")
	}
	if len(cal.Points) != 1 {
		t.Errorf("expected 1 point, got %d", len(cal.Points))
	}
	if cal.PointCount != 1 {
		t.Errorf("expected point count 1, got %d", cal.PointCount)
	}
	if len(cal.TapPoints["tap-1"]) != 1 {
		t.Errorf("expected 1 tap point, got %d", len(cal.TapPoints["tap-1"]))
	}
}

func TestRecordCalibrationPoint_RejectsEmptyModel(t *testing.T) {
	lc := newTestCalibrator()
	lc.RecordCalibrationPoint("", "tap-1", -75.0, 1500.0)
	if len(lc.models) != 0 {
		t.Error("should reject empty model")
	}
}

func TestRecordCalibrationPoint_RejectsEmptyTap(t *testing.T) {
	lc := newTestCalibrator()
	lc.RecordCalibrationPoint("Air 2S", "", -75.0, 1500.0)
	if len(lc.models) != 0 {
		t.Error("should reject empty tap")
	}
}

func TestRecordCalibrationPoint_RejectsRSSITooLow(t *testing.T) {
	lc := newTestCalibrator()
	lc.RecordCalibrationPoint("Air 2S", "tap-1", -125.0, 1500.0)
	if len(lc.models) != 0 {
		t.Error("should reject RSSI below -120")
	}
}

func TestRecordCalibrationPoint_RejectsRSSITooHigh(t *testing.T) {
	lc := newTestCalibrator()
	lc.RecordCalibrationPoint("Air 2S", "tap-1", -20.0, 1500.0)
	if len(lc.models) != 0 {
		t.Error("should reject RSSI above -25")
	}
}

func TestRecordCalibrationPoint_RejectsRSSIPlaceholder(t *testing.T) {
	lc := newTestCalibrator()
	lc.RecordCalibrationPoint("Air 2S", "tap-1", RSSIPlaceholder, 1500.0)
	if len(lc.models) != 0 {
		t.Error("should reject RSSI placeholder value")
	}
}

func TestRecordCalibrationPoint_RejectsDistanceTooClose(t *testing.T) {
	lc := newTestCalibrator()
	lc.RecordCalibrationPoint("Air 2S", "tap-1", -75.0, 10.0)
	if len(lc.models) != 0 {
		t.Error("should reject distance below 50m")
	}
}

func TestRecordCalibrationPoint_RejectsDistanceTooFar(t *testing.T) {
	lc := newTestCalibrator()
	lc.RecordCalibrationPoint("Air 2S", "tap-1", -75.0, 20000.0)
	if len(lc.models) != 0 {
		t.Error("should reject distance above 15000m")
	}
}

func TestRecordCalibrationPoint_RSSI0BackCalculation(t *testing.T) {
	lc := newTestCalibrator()
	// Known values: RSSI=-75, distance=1000m, n=DefaultPathLossN (1.8)
	// RSSI_0 = -75 + 10 * 1.8 * log10(1000) = -75 + 54 = -21
	lc.RecordCalibrationPoint("TestModel", "tap-1", -75.0, 1000.0)

	cal := lc.models["TestModel"]
	if cal == nil {
		t.Fatal("model not recorded")
	}
	expectedRSSI0 := -75.0 + 10*DefaultPathLossN*math.Log10(1000.0)
	actual := cal.Points[0].RSSI0
	if math.Abs(actual-expectedRSSI0) > 0.01 {
		t.Errorf("RSSI_0 back-calculation: expected %.2f, got %.2f", expectedRSSI0, actual)
	}
}

func TestRecordCalibrationPoint_SlidingWindow(t *testing.T) {
	lc := newTestCalibrator()

	// Fill beyond maxCalPoints
	for i := 0; i < maxCalPoints+50; i++ {
		lc.RecordCalibrationPoint("TestModel", "tap-1", -75.0, 1000.0)
	}

	cal := lc.models["TestModel"]
	if len(cal.Points) != maxCalPoints {
		t.Errorf("expected sliding window to cap at %d, got %d", maxCalPoints, len(cal.Points))
	}
	if len(cal.TapPoints["tap-1"]) != maxCalPoints {
		t.Errorf("expected tap sliding window to cap at %d, got %d", maxCalPoints, len(cal.TapPoints["tap-1"]))
	}
	if cal.PointCount != maxCalPoints+50 {
		t.Errorf("expected total point count %d, got %d", maxCalPoints+50, cal.PointCount)
	}
}

func TestRecordCalibrationPoint_MultipleTaps(t *testing.T) {
	lc := newTestCalibrator()

	feedCalibrationPoints(lc, "Air 2S", "tap-1", 5, -75.0, 1500.0)
	feedCalibrationPoints(lc, "Air 2S", "tap-2", 5, -80.0, 2000.0)

	cal := lc.models["Air 2S"]
	if len(cal.Points) != 10 {
		t.Errorf("expected 10 global points, got %d", len(cal.Points))
	}
	if len(cal.TapPoints["tap-1"]) != 5 {
		t.Errorf("expected 5 points for tap-1, got %d", len(cal.TapPoints["tap-1"]))
	}
	if len(cal.TapPoints["tap-2"]) != 5 {
		t.Errorf("expected 5 points for tap-2, got %d", len(cal.TapPoints["tap-2"]))
	}
	if len(cal.TapRSSI0) != 2 {
		t.Errorf("expected 2 per-tap RSSI_0 values, got %d", len(cal.TapRSSI0))
	}
}

func TestRecordCalibrationPoint_RefitsAfterMinPoints(t *testing.T) {
	lc := newTestCalibrator()

	// Add fewer than calMinPoints - should not refit
	feedCalibrationPoints(lc, "TestModel", "tap-1", calMinPoints-1, -75.0, 1000.0)
	cal := lc.models["TestModel"]
	if cal.LiveRSSI0 != 0 {
		t.Errorf("should not refit with fewer than %d points, got RSSI_0=%f", calMinPoints, cal.LiveRSSI0)
	}

	// Add one more to hit threshold
	lc.RecordCalibrationPoint("TestModel", "tap-1", -75.0, 1000.0)
	cal = lc.models["TestModel"]
	if cal.LiveRSSI0 == 0 {
		t.Error("should have refitted after reaching minimum points")
	}
}

// === Tests: RememberIdentity ===

func TestRememberIdentity_Basic(t *testing.T) {
	lc := newTestCalibrator()
	lc.RememberIdentity("drone-123", "Air 2S", "SN12345")

	if len(lc.identities) != 1 {
		t.Fatalf("expected 1 identity, got %d", len(lc.identities))
	}
	mem := lc.identities["drone-123"]
	if mem.Model != "Air 2S" || mem.Serial != "SN12345" {
		t.Errorf("identity mismatch: model=%s serial=%s", mem.Model, mem.Serial)
	}
}

func TestRememberIdentity_RejectsEmpty(t *testing.T) {
	lc := newTestCalibrator()
	lc.RememberIdentity("", "Air 2S", "SN123")
	lc.RememberIdentity("drone-123", "", "SN123")
	if len(lc.identities) != 0 {
		t.Error("should reject empty identifier or model")
	}
}

func TestRememberIdentity_UpdatesExisting(t *testing.T) {
	lc := newTestCalibrator()
	lc.RememberIdentity("drone-123", "Air 2S", "SN12345")
	lc.RememberIdentity("drone-123", "Mavic 3", "SN99999")

	if len(lc.identities) != 1 {
		t.Fatalf("expected 1 identity (update), got %d", len(lc.identities))
	}
	mem := lc.identities["drone-123"]
	if mem.Model != "Mavic 3" || mem.Serial != "SN99999" {
		t.Errorf("identity not updated: model=%s serial=%s", mem.Model, mem.Serial)
	}
}

func TestRememberIdentity_MaxCapEnforced(t *testing.T) {
	lc := newTestCalibrator()

	// Fill to max capacity
	for i := 0; i < maxIdentities; i++ {
		lc.RememberIdentity(
			"drone-"+string(rune(i))+"_"+time.Now().String(),
			"Model",
			"",
		)
	}

	// Adding one more should trigger eviction (removes 10%)
	lc.RememberIdentity("overflow-drone", "Air 2S", "")

	if len(lc.identities) > maxIdentities {
		t.Errorf("identities exceeded max cap: %d > %d", len(lc.identities), maxIdentities)
	}
}

// === Tests: RecallModel ===

func TestRecallModel_Found(t *testing.T) {
	lc := newTestCalibrator()
	lc.RememberIdentity("drone-123", "Air 2S", "SN12345")

	model, serial, ok := lc.RecallModel("drone-123")
	if !ok {
		t.Fatal("expected to find identity")
	}
	if model != "Air 2S" || serial != "SN12345" {
		t.Errorf("recalled wrong data: model=%s serial=%s", model, serial)
	}
}

func TestRecallModel_NotFound(t *testing.T) {
	lc := newTestCalibrator()
	_, _, ok := lc.RecallModel("nonexistent")
	if ok {
		t.Error("should not find nonexistent identity")
	}
}

func TestRecallModel_ExpiredIdentity(t *testing.T) {
	lc := newTestCalibrator()

	// Manually insert an old identity
	lc.mu.Lock()
	lc.identities["old-drone"] = &IdentityMemory{
		Model:    "Air 2S",
		Serial:   "SN123",
		LastSeen: time.Now().Add(-25 * time.Hour), // older than calPointMaxAge
	}
	lc.mu.Unlock()

	_, _, ok := lc.RecallModel("old-drone")
	if ok {
		t.Error("should not recall expired identity (>24h old)")
	}
}

// === Tests: GetCalibratedRSSI0 ===

func TestGetCalibratedRSSI0_LiveTap(t *testing.T) {
	lc := newTestCalibrator()
	feedCalibrationPoints(lc, "Air 2S", "tap-1", calMinPoints+2, -75.0, 1500.0)

	rssi0, source := lc.GetCalibratedRSSI0("Air 2S", "tap-1")
	if source != "live_tap" {
		t.Errorf("expected source 'live_tap', got '%s'", source)
	}
	if rssi0 == 0 {
		t.Error("RSSI_0 should not be zero for live tap calibration")
	}
}

func TestGetCalibratedRSSI0_LiveAll(t *testing.T) {
	lc := newTestCalibrator()
	// Add points for tap-1 but query for tap-2 (which has no data)
	feedCalibrationPoints(lc, "Air 2S", "tap-1", calMinPoints+2, -75.0, 1500.0)

	rssi0, source := lc.GetCalibratedRSSI0("Air 2S", "tap-2")
	if source != "live_all" {
		t.Errorf("expected source 'live_all', got '%s'", source)
	}
	if rssi0 == 0 {
		t.Error("RSSI_0 should not be zero for live all calibration")
	}
}

func TestGetCalibratedRSSI0_StaticModel(t *testing.T) {
	lc := newTestCalibrator()
	// No live data, but "Air 2S" has a static offset in ModelTXOffsets
	rssi0, source := lc.GetCalibratedRSSI0("Air 2S", "tap-1")
	if source != "static_model" {
		t.Errorf("expected source 'static_model', got '%s'", source)
	}
	expected := BaseRSSI0 + ModelTXOffsets["Air 2S"]
	if math.Abs(rssi0-expected) > 0.01 {
		t.Errorf("expected RSSI_0 %.2f, got %.2f", expected, rssi0)
	}
}

func TestGetCalibratedRSSI0_GenericFallback(t *testing.T) {
	lc := newTestCalibrator()
	rssi0, source := lc.GetCalibratedRSSI0("TotallyUnknownModel", "tap-1")
	if source != "generic" {
		t.Errorf("expected source 'generic', got '%s'", source)
	}
	if rssi0 != GenericRSSI0 {
		t.Errorf("expected generic RSSI_0 %f, got %f", GenericRSSI0, rssi0)
	}
}

func TestGetCalibratedRSSI0_EmptyModel(t *testing.T) {
	lc := newTestCalibrator()
	rssi0, source := lc.GetCalibratedRSSI0("", "tap-1")
	if source != "generic" {
		t.Errorf("expected source 'generic', got '%s'", source)
	}
	if rssi0 != GenericRSSI0 {
		t.Errorf("expected generic RSSI_0 for empty model")
	}
}

func TestGetCalibratedRSSI0_PriorityChain(t *testing.T) {
	lc := newTestCalibrator()

	// Step 1: No data - should use static model offset
	_, source := lc.GetCalibratedRSSI0("Air 2S", "tap-1")
	if source != "static_model" {
		t.Errorf("step 1: expected static_model, got %s", source)
	}

	// Step 2: Add data for different tap - should use live_all
	feedCalibrationPoints(lc, "Air 2S", "tap-2", calMinPoints, -75.0, 1500.0)
	_, source = lc.GetCalibratedRSSI0("Air 2S", "tap-1")
	if source != "live_all" {
		t.Errorf("step 2: expected live_all, got %s", source)
	}

	// Step 3: Add data for the specific tap - should use live_tap
	feedCalibrationPoints(lc, "Air 2S", "tap-1", calMinPoints, -80.0, 2000.0)
	_, source = lc.GetCalibratedRSSI0("Air 2S", "tap-1")
	if source != "live_tap" {
		t.Errorf("step 3: expected live_tap, got %s", source)
	}
}

// === Tests: EstimateDistanceLive ===

func TestEstimateDistanceLive_ValidRSSI(t *testing.T) {
	lc := newTestCalibrator()
	dist, conf, source := lc.EstimateDistanceLive(-75.0, "Air 2S", "tap-1")
	if dist <= 0 {
		t.Error("expected positive distance")
	}
	if conf <= 0 || conf > 1 {
		t.Errorf("expected confidence in (0,1], got %f", conf)
	}
	if source == "" {
		t.Error("expected non-empty source")
	}
}

func TestEstimateDistanceLive_InvalidRSSI(t *testing.T) {
	lc := newTestCalibrator()

	// Positive RSSI
	dist, _, _ := lc.EstimateDistanceLive(5.0, "Air 2S", "tap-1")
	if dist != -1 {
		t.Error("should return -1 for positive RSSI")
	}

	// Below minimum
	dist, _, _ = lc.EstimateDistanceLive(-130.0, "Air 2S", "tap-1")
	if dist != -1 {
		t.Error("should return -1 for RSSI below minimum")
	}

	// Above maximum
	dist, _, _ = lc.EstimateDistanceLive(-15.0, "Air 2S", "tap-1")
	if dist != -1 {
		t.Error("should return -1 for RSSI above maximum")
	}
}

func TestEstimateDistanceLive_DistanceClamping(t *testing.T) {
	lc := newTestCalibrator()

	// Very strong signal should clamp to MinDistanceM
	dist, _, _ := lc.EstimateDistanceLive(-25.0, "", "")
	if dist < MinDistanceM {
		t.Errorf("distance %f should be >= MinDistanceM %f", dist, MinDistanceM)
	}

	// Very weak signal should clamp to MaxDistanceM
	dist, _, _ = lc.EstimateDistanceLive(-119.0, "", "")
	if dist > MaxDistanceM {
		t.Errorf("distance %f should be <= MaxDistanceM %f", dist, MaxDistanceM)
	}
}

func TestEstimateDistanceLive_ConfidenceBySource(t *testing.T) {
	lc := newTestCalibrator()

	// Generic (no model)
	_, conf, _ := lc.EstimateDistanceLive(-75.0, "", "")
	if conf != 0.35 {
		t.Errorf("generic confidence should be 0.35, got %f", conf)
	}

	// Static model
	_, conf, _ = lc.EstimateDistanceLive(-75.0, "Air 2S", "tap-1")
	if conf != 0.60 {
		t.Errorf("static_model confidence should be 0.60, got %f", conf)
	}

	// Live all
	feedCalibrationPoints(lc, "Air 2S", "tap-2", calMinPoints+2, -75.0, 1500.0)
	_, conf, source := lc.EstimateDistanceLive(-75.0, "Air 2S", "tap-1")
	if source != "live_all" {
		t.Fatalf("expected live_all, got %s", source)
	}
	if conf != 0.75 {
		t.Errorf("live_all confidence should be 0.75, got %f", conf)
	}

	// Live tap
	feedCalibrationPoints(lc, "Air 2S", "tap-1", calMinPoints+2, -80.0, 2000.0)
	_, conf, source = lc.EstimateDistanceLive(-75.0, "Air 2S", "tap-1")
	if source != "live_tap" {
		t.Fatalf("expected live_tap, got %s", source)
	}
	if conf != 0.85 {
		t.Errorf("live_tap confidence should be 0.85, got %f", conf)
	}
}

// === Tests: EstimateDistanceLiveWithBounds ===

func TestEstimateDistanceLiveWithBounds_ValidResult(t *testing.T) {
	lc := newTestCalibrator()
	result := lc.EstimateDistanceLiveWithBounds(-75.0, "Air 2S", "tap-1")

	if result.DistanceM <= 0 {
		t.Fatal("expected positive distance")
	}
	if result.DistanceMinM >= result.DistanceM {
		t.Errorf("min (%.1f) should be < distance (%.1f)", result.DistanceMinM, result.DistanceM)
	}
	if result.DistanceMaxM <= result.DistanceM {
		t.Errorf("max (%.1f) should be > distance (%.1f)", result.DistanceMaxM, result.DistanceM)
	}
	if result.Confidence <= 0 || result.Confidence > 1 {
		t.Errorf("confidence %.2f should be in (0,1]", result.Confidence)
	}
	if result.ModelUsed == "" {
		t.Error("ModelUsed should not be empty")
	}
	if result.Environment == "" {
		t.Error("Environment should not be empty")
	}
}

func TestEstimateDistanceLiveWithBounds_InvalidRSSI(t *testing.T) {
	lc := newTestCalibrator()
	result := lc.EstimateDistanceLiveWithBounds(5.0, "Air 2S", "tap-1")
	if result.DistanceM != 0 {
		t.Error("expected zero distance for invalid RSSI")
	}
}

func TestEstimateDistanceLiveWithBounds_TighterBoundsForLiveData(t *testing.T) {
	lc := newTestCalibrator()

	// Generic bounds (no model)
	generic := lc.EstimateDistanceLiveWithBounds(-75.0, "", "tap-1")
	genericSpread := generic.DistanceMaxM - generic.DistanceMinM

	// Feed live calibration data
	feedCalibrationPoints(lc, "TestModel", "tap-1", calMinPoints+5, -75.0, 1500.0)

	// Live tap relative bounds should be tighter (lower percentage spread)
	live := lc.EstimateDistanceLiveWithBounds(-75.0, "TestModel", "tap-1")
	liveRelSpread := (live.DistanceMaxM - live.DistanceMinM) / live.DistanceM
	genericRelSpread := genericSpread / generic.DistanceM

	if liveRelSpread >= genericRelSpread {
		t.Errorf("live relative spread (%.2f) should be tighter than generic (%.2f)",
			liveRelSpread, genericRelSpread)
	}
}

func TestEstimateDistanceLiveWithBounds_ModelUsedFormat(t *testing.T) {
	lc := newTestCalibrator()

	// With model
	result := lc.EstimateDistanceLiveWithBounds(-75.0, "Air 2S", "tap-1")
	if result.ModelUsed != "Air 2S [static_model]" {
		t.Errorf("expected 'Air 2S [static_model]', got '%s'", result.ModelUsed)
	}

	// Without model (should say "unknown" not leading space)
	result = lc.EstimateDistanceLiveWithBounds(-75.0, "", "tap-1")
	if result.ModelUsed != "unknown [generic]" {
		t.Errorf("expected 'unknown [generic]', got '%s'", result.ModelUsed)
	}
}

// === Tests: PruneStale ===

func TestPruneStale_RemovesOldPoints(t *testing.T) {
	lc := newTestCalibrator()

	// Add fresh points
	feedCalibrationPoints(lc, "Air 2S", "tap-1", 3, -75.0, 1500.0)

	// Manually inject old points
	lc.mu.Lock()
	cal := lc.models["Air 2S"]
	oldPoint := CalibrationPoint{
		RSSI:      -80,
		DistanceM: 2000,
		RSSI0:     -20,
		TapID:     "tap-1",
		Timestamp: time.Now().Add(-25 * time.Hour),
	}
	cal.Points = append(cal.Points, oldPoint)
	cal.TapPoints["tap-1"] = append(cal.TapPoints["tap-1"], oldPoint)
	lc.mu.Unlock()

	// Verify old point was added
	if len(lc.models["Air 2S"].Points) != 4 {
		t.Fatalf("expected 4 points before prune, got %d", len(lc.models["Air 2S"].Points))
	}

	lc.PruneStale()

	if len(lc.models["Air 2S"].Points) != 3 {
		t.Errorf("expected 3 points after prune, got %d", len(lc.models["Air 2S"].Points))
	}
}

func TestPruneStale_RemovesEmptyModels(t *testing.T) {
	lc := newTestCalibrator()

	// Manually insert model with only old points
	lc.mu.Lock()
	lc.models["OldModel"] = &ModelCalibration{
		Model: "OldModel",
		Points: []CalibrationPoint{
			{Timestamp: time.Now().Add(-25 * time.Hour)},
		},
		TapPoints: map[string][]CalibrationPoint{
			"tap-1": {{Timestamp: time.Now().Add(-25 * time.Hour)}},
		},
		TapRSSI0: map[string]float64{"tap-1": -30},
	}
	lc.mu.Unlock()

	lc.PruneStale()

	if _, exists := lc.models["OldModel"]; exists {
		t.Error("should have removed model with all old points")
	}
}

func TestPruneStale_RemovesOldIdentities(t *testing.T) {
	lc := newTestCalibrator()
	lc.RememberIdentity("fresh-drone", "Air 2S", "SN1")

	// Manually insert old identity
	lc.mu.Lock()
	lc.identities["old-drone"] = &IdentityMemory{
		Model:    "Mavic 3",
		LastSeen: time.Now().Add(-49 * time.Hour), // >48h
	}
	lc.mu.Unlock()

	lc.PruneStale()

	if _, exists := lc.identities["old-drone"]; exists {
		t.Error("should have pruned identity older than 48h")
	}
	if _, exists := lc.identities["fresh-drone"]; !exists {
		t.Error("should have kept fresh identity")
	}
}

func TestPruneStale_RemovesEmptyTapEntries(t *testing.T) {
	lc := newTestCalibrator()

	// Add fresh data for tap-1, old data for tap-2
	feedCalibrationPoints(lc, "Air 2S", "tap-1", 5, -75.0, 1500.0)

	lc.mu.Lock()
	cal := lc.models["Air 2S"]
	cal.TapPoints["tap-2"] = []CalibrationPoint{
		{Timestamp: time.Now().Add(-25 * time.Hour)},
	}
	cal.TapRSSI0["tap-2"] = -30
	lc.mu.Unlock()

	lc.PruneStale()

	cal = lc.models["Air 2S"]
	if _, exists := cal.TapPoints["tap-2"]; exists {
		t.Error("should have removed tap-2 with all old points")
	}
	if _, exists := cal.TapRSSI0["tap-2"]; exists {
		t.Error("should have removed tap-2 RSSI_0 entry")
	}
	if _, exists := cal.TapPoints["tap-1"]; !exists {
		t.Error("should have kept tap-1 with fresh points")
	}
}

// === Tests: GetStats ===

func TestGetStats_EmptyCalibrator(t *testing.T) {
	lc := newTestCalibrator()
	stats := lc.GetStats()

	if stats["models_calibrated"].(int) != 0 {
		t.Error("expected 0 models calibrated")
	}
	if stats["identities_cached"].(int) != 0 {
		t.Error("expected 0 identities cached")
	}
	if stats["path_loss_n"].(float64) != DefaultPathLossN {
		t.Error("expected default path loss")
	}
}

func TestGetStats_WithData(t *testing.T) {
	lc := newTestCalibrator()
	feedCalibrationPoints(lc, "Air 2S", "tap-1", 10, -75.0, 1500.0)
	lc.RememberIdentity("drone-1", "Air 2S", "SN1")
	lc.RememberIdentity("drone-2", "Mavic 3", "SN2")

	stats := lc.GetStats()

	if stats["models_calibrated"].(int) != 1 {
		t.Errorf("expected 1 model calibrated, got %d", stats["models_calibrated"].(int))
	}
	if stats["identities_cached"].(int) != 2 {
		t.Errorf("expected 2 identities, got %d", stats["identities_cached"].(int))
	}

	models := stats["models"].([]map[string]interface{})
	if len(models) != 1 {
		t.Fatalf("expected 1 model stat, got %d", len(models))
	}
	if models[0]["model"].(string) != "Air 2S" {
		t.Errorf("expected model 'Air 2S', got '%s'", models[0]["model"].(string))
	}
	if models[0]["total_points"].(int) != 10 {
		t.Errorf("expected 10 total points, got %d", models[0]["total_points"].(int))
	}
}

// === Tests: fitRSSI0 ===

func TestFitRSSI0_MedianCalculation(t *testing.T) {
	lc := newTestCalibrator()

	// Create points with known RSSI_0 values
	now := time.Now()
	points := []CalibrationPoint{
		{RSSI0: -20, Timestamp: now},
		{RSSI0: -22, Timestamp: now},
		{RSSI0: -18, Timestamp: now},
		{RSSI0: -25, Timestamp: now},
		{RSSI0: -19, Timestamp: now},
	}

	result := lc.fitRSSI0(points)
	// Sorted: -25, -22, -20, -19, -18 -> median = -20
	if result != -20.0 {
		t.Errorf("expected median -20, got %f", result)
	}
}

func TestFitRSSI0_OldPointsIgnored(t *testing.T) {
	lc := newTestCalibrator()
	now := time.Now()

	points := []CalibrationPoint{
		{RSSI0: -20, Timestamp: now},
		{RSSI0: -22, Timestamp: now},
		{RSSI0: -100, Timestamp: now.Add(-25 * time.Hour)}, // Old - should be ignored
	}

	result := lc.fitRSSI0(points)
	// Only fresh points: -22, -20 -> median = -21
	expected := (-22.0 + -20.0) / 2.0
	if math.Abs(result-expected) > 0.01 {
		t.Errorf("expected median %.2f (ignoring old points), got %.2f", expected, result)
	}
}

func TestFitRSSI0_AllOldReturnsGeneric(t *testing.T) {
	lc := newTestCalibrator()

	points := []CalibrationPoint{
		{RSSI0: -20, Timestamp: time.Now().Add(-25 * time.Hour)},
	}

	result := lc.fitRSSI0(points)
	if result != GenericRSSI0 {
		t.Errorf("expected GenericRSSI0 when all points are old, got %f", result)
	}
}

func TestFitRSSI0_EmptyReturnsGeneric(t *testing.T) {
	lc := newTestCalibrator()
	result := lc.fitRSSI0(nil)
	if result != GenericRSSI0 {
		t.Errorf("expected GenericRSSI0 for nil points, got %f", result)
	}
}

// === Tests: median ===

func TestMedian_OddCount(t *testing.T) {
	result := median([]float64{3, 1, 2})
	if result != 2 {
		t.Errorf("expected median 2, got %f", result)
	}
}

func TestMedian_EvenCount(t *testing.T) {
	result := median([]float64{4, 1, 3, 2})
	if result != 2.5 {
		t.Errorf("expected median 2.5, got %f", result)
	}
}

func TestMedian_SingleValue(t *testing.T) {
	result := median([]float64{42})
	if result != 42 {
		t.Errorf("expected 42, got %f", result)
	}
}

func TestMedian_Empty(t *testing.T) {
	result := median([]float64{})
	if result != 0 {
		t.Errorf("expected 0 for empty, got %f", result)
	}
}

func TestMedian_DoesNotMutateOriginal(t *testing.T) {
	original := []float64{3, 1, 4, 1, 5}
	copy := make([]float64, len(original))
	for i, v := range original {
		copy[i] = v
	}

	median(original)

	for i := range original {
		if original[i] != copy[i] {
			t.Errorf("median mutated original at index %d: %f -> %f", i, copy[i], original[i])
		}
	}
}

func TestMedian_WithNegatives(t *testing.T) {
	// Typical RSSI_0 values
	result := median([]float64{-22, -18, -20, -25, -19})
	// Sorted: -25, -22, -20, -19, -18 -> median = -20
	if result != -20 {
		t.Errorf("expected median -20, got %f", result)
	}
}

// === Tests: HaversineDistance ===

func TestHaversineDistance_SamePoint(t *testing.T) {
	dist := HaversineDistance(18.4655, -66.1057, 18.4655, -66.1057)
	if dist != 0 {
		t.Errorf("same point should be 0, got %f", dist)
	}
}

func TestHaversineDistance_KnownDistance(t *testing.T) {
	// New York to Los Angeles ~3944km
	dist := HaversineDistance(40.7128, -74.0060, 34.0522, -118.2437)
	expected := 3944000.0
	tolerance := 50000.0 // 50km tolerance
	if math.Abs(dist-expected) > tolerance {
		t.Errorf("NY to LA: expected ~%.0fm, got %.0fm", expected, dist)
	}
}

func TestHaversineDistance_ShortDistance(t *testing.T) {
	// Two points ~1km apart near equator
	// 1 degree latitude ≈ 111km, so 0.009 degrees ≈ 1km
	dist := HaversineDistance(0.0, 0.0, 0.009, 0.0)
	expected := 1000.0
	tolerance := 10.0 // 10m tolerance
	if math.Abs(dist-expected) > tolerance {
		t.Errorf("short distance: expected ~%.0fm, got %.0fm", expected, dist)
	}
}

func TestHaversineDistance_Symmetric(t *testing.T) {
	d1 := HaversineDistance(18.0, -66.0, 19.0, -67.0)
	d2 := HaversineDistance(19.0, -67.0, 18.0, -66.0)
	if math.Abs(d1-d2) > 0.001 {
		t.Errorf("haversine should be symmetric: %f vs %f", d1, d2)
	}
}

// === Tests: formatModelSource ===

func TestFormatModelSource_WithModel(t *testing.T) {
	result := formatModelSource("Air 2S", "live_tap")
	if result != "Air 2S [live_tap]" {
		t.Errorf("expected 'Air 2S [live_tap]', got '%s'", result)
	}
}

func TestFormatModelSource_EmptyModel(t *testing.T) {
	result := formatModelSource("", "generic")
	if result != "unknown [generic]" {
		t.Errorf("expected 'unknown [generic]', got '%s'", result)
	}
}

// === Tests: evictOldestIdentities ===

func TestEvictOldestIdentities_Basic(t *testing.T) {
	lc := newTestCalibrator()

	// Add identities with staggered timestamps
	lc.mu.Lock()
	for i := 0; i < 100; i++ {
		lc.identities[string(rune('A'+i))] = &IdentityMemory{
			Model:    "Model",
			LastSeen: time.Now().Add(-time.Duration(i) * time.Minute),
		}
	}
	lc.mu.Unlock()

	lc.mu.Lock()
	lc.evictOldestIdentities(10)
	lc.mu.Unlock()

	if len(lc.identities) != 90 {
		t.Errorf("expected 90 after evicting 10, got %d", len(lc.identities))
	}
}

func TestEvictOldestIdentities_ZeroCount(t *testing.T) {
	lc := newTestCalibrator()
	lc.RememberIdentity("drone-1", "Air 2S", "")

	lc.mu.Lock()
	lc.evictOldestIdentities(0)
	lc.mu.Unlock()

	if len(lc.identities) != 1 {
		t.Error("zero count should not evict anything")
	}
}

// === Tests: Concurrent access ===

func TestConcurrentAccess(t *testing.T) {
	lc := newTestCalibrator()
	var wg sync.WaitGroup

	// Concurrent calibration point recording
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(tapID string) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				lc.RecordCalibrationPoint("Air 2S", tapID, -75.0, 1500.0)
			}
		}(string(rune('A' + i)))
	}

	// Concurrent identity operations
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				lc.RememberIdentity(string(rune('a'+id)), "Model", "")
				lc.RecallModel(string(rune('a' + id)))
			}
		}(i)
	}

	// Concurrent reads
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				lc.GetCalibratedRSSI0("Air 2S", "A")
				lc.EstimateDistanceLive(-75.0, "Air 2S", "A")
				lc.GetStats()
			}
		}()
	}

	// Concurrent pruning
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			lc.PruneStale()
		}
	}()

	wg.Wait()

	// If we get here without a race detector panic, concurrency is correct
	stats := lc.GetStats()
	if stats["models_calibrated"].(int) < 1 {
		t.Error("expected at least 1 model after concurrent recording")
	}
}

// === Tests: End-to-end calibration flow ===

func TestEndToEnd_CalibrationImprovement(t *testing.T) {
	lc := newTestCalibrator()

	// Phase 1: Before calibration, use static offset
	_, _, source1 := lc.EstimateDistanceLive(-80.0, "Air 2S", "tap-1")
	if source1 != "static_model" {
		t.Errorf("before calibration: expected static_model, got %s", source1)
	}

	// Phase 2: Feed ground truth GPS+RSSI pairs
	// Simulating Air 2S at various distances from tap-1
	testPairs := []struct{ rssi, distance float64 }{
		{-60.0, 200.0},
		{-65.0, 500.0},
		{-70.0, 800.0},
		{-75.0, 1500.0},
		{-80.0, 2500.0},
		{-85.0, 4000.0},
		{-88.0, 6000.0},
	}
	for _, pair := range testPairs {
		lc.RecordCalibrationPoint("Air 2S", "tap-1", pair.rssi, pair.distance)
	}

	// Phase 3: Now should use live_tap calibration
	_, _, source2 := lc.EstimateDistanceLive(-80.0, "Air 2S", "tap-1")
	if source2 != "live_tap" {
		t.Errorf("after calibration: expected live_tap, got %s", source2)
	}

	// Phase 4: Query for a different tap - should use live_all
	_, _, source3 := lc.EstimateDistanceLive(-80.0, "Air 2S", "tap-2")
	if source3 != "live_all" {
		t.Errorf("different tap: expected live_all, got %s", source3)
	}
}

func TestEndToEnd_DarkDroneTracking(t *testing.T) {
	lc := newTestCalibrator()

	// Phase 1: Drone broadcasts RemoteID with GPS
	lc.RememberIdentity("drone-abc", "Air 2S", "SN12345")
	lc.RememberIdentity("AA:BB:CC:DD:EE:FF", "Air 2S", "SN12345")

	// Feed calibration data
	feedCalibrationPoints(lc, "Air 2S", "tap-1", calMinPoints+3, -75.0, 1500.0)

	// Phase 2: Drone goes dark (kills RemoteID) - we only have identifier/MAC
	model, serial, ok := lc.RecallModel("drone-abc")
	if !ok {
		t.Fatal("should recall model for known identifier")
	}
	if model != "Air 2S" || serial != "SN12345" {
		t.Errorf("wrong recall: model=%s serial=%s", model, serial)
	}

	// Also recall by MAC
	model, _, ok = lc.RecallModel("AA:BB:CC:DD:EE:FF")
	if !ok {
		t.Fatal("should recall model by MAC")
	}
	if model != "Air 2S" {
		t.Errorf("MAC recall: expected Air 2S, got %s", model)
	}

	// Phase 3: Use recalled model for live-calibrated distance estimation
	dist, conf, source := lc.EstimateDistanceLive(-80.0, model, "tap-1")
	if source != "live_tap" {
		t.Errorf("dark drone should use live_tap calibration, got %s", source)
	}
	if dist <= 0 {
		t.Error("dark drone should get valid distance estimate")
	}
	if conf != 0.85 {
		t.Errorf("dark drone with live_tap should have 0.85 confidence, got %f", conf)
	}
}

// === Benchmarks ===

func BenchmarkRecordCalibrationPoint(b *testing.B) {
	lc := newTestCalibrator()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lc.RecordCalibrationPoint("Air 2S", "tap-1", -75.0, 1500.0)
	}
}

func BenchmarkEstimateDistanceLive(b *testing.B) {
	lc := newTestCalibrator()
	feedCalibrationPoints(lc, "Air 2S", "tap-1", 50, -75.0, 1500.0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lc.EstimateDistanceLive(-75.0, "Air 2S", "tap-1")
	}
}

func BenchmarkGetCalibratedRSSI0(b *testing.B) {
	lc := newTestCalibrator()
	feedCalibrationPoints(lc, "Air 2S", "tap-1", 50, -75.0, 1500.0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lc.GetCalibratedRSSI0("Air 2S", "tap-1")
	}
}

func BenchmarkRememberIdentity(b *testing.B) {
	lc := newTestCalibrator()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lc.RememberIdentity("drone-"+string(rune(i%1000)), "Air 2S", "SN123")
	}
}

func BenchmarkPruneStale(b *testing.B) {
	lc := newTestCalibrator()
	// Fill with data
	for i := 0; i < 20; i++ {
		feedCalibrationPoints(lc, "Model-"+string(rune('A'+i)), "tap-1", maxCalPoints, -75.0, 1500.0)
	}
	for i := 0; i < 1000; i++ {
		lc.RememberIdentity("drone-"+string(rune(i)), "Model", "")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lc.PruneStale()
	}
}
