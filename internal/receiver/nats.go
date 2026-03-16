package receiver

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/K13094/skylens/internal/intel"
	"github.com/K13094/skylens/internal/processor"
	"github.com/K13094/skylens/internal/storage"
	pb "github.com/K13094/skylens/proto"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

// debugStack returns the current goroutine's stack trace for panic recovery logging
func debugStack() []byte { return debug.Stack() }

// DB worker pool defaults
const (
	defaultDBWorkerCount = 8    // Parallel DB writers
	defaultDBQueueSize   = 5000 // Buffer for burst traffic
	dbMaxRetries         = 2    // Max retries on DB save failure
)

// cachedTapPos holds a TAP's position, refreshed on each heartbeat (~5s).
type cachedTapPos struct {
	Latitude  float64
	Longitude float64
}

// getTapPos returns cached TAP position, falling back to state manager.
func (r *Receiver) getTapPos(tapID string) (lat, lon float64, ok bool) {
	if v, found := r.tapCache.Load(tapID); found {
		pos := v.(*cachedTapPos)
		return pos.Latitude, pos.Longitude, true
	}
	// Fallback to state manager (cold start before first heartbeat)
	if tap, found := r.state.GetTap(tapID); found {
		return tap.Latitude, tap.Longitude, true
	}
	return 0, 0, false
}

// isGenericDesignation checks if a designation is empty or a generic placeholder
func isGenericDesignation(s string) bool {
	return s == "" || s == "WiFi RemoteID broadcast" || s == "UNKNOWN" || s == "Unknown"
}

// isValidIdentifier checks if a string contains only printable ASCII
func isValidIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7E {
			return false
		}
	}
	return true
}

// isCleanSerial checks if a serial number contains only printable ASCII characters
func isCleanSerial(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7E {
			return false
		}
	}
	return true
}

// detectionSourceDisplayName converts a DetectionSource enum to a human-readable string
// normalizeOpStatus converts protobuf OperationalStatus string to a known value.
// Protobuf returns the raw numeric string for out-of-range enum values (e.g. "15").
func normalizeOpStatus(s string) string {
	switch s {
	case "OP_STATUS_UNKNOWN", "OP_STATUS_UNDECLARED", "OP_STATUS_GROUND",
		"OP_STATUS_AIRBORNE", "OP_STATUS_EMERGENCY", "OP_STATUS_REMOTE_ID_FAILURE":
		return s
	default:
		return "OP_STATUS_UNKNOWN"
	}
}

func detectionSourceDisplayName(src pb.DetectionSource) string {
	switch src {
	case pb.DetectionSource_SOURCE_WIFI_BEACON:
		return "WiFi Beacon"
	case pb.DetectionSource_SOURCE_WIFI_NAN:
		return "WiFi NAN"
	case pb.DetectionSource_SOURCE_WIFI_PROBE_RESP:
		return "WiFi Probe"
	case pb.DetectionSource_SOURCE_BLUETOOTH_4:
		return "Bluetooth 4"
	case pb.DetectionSource_SOURCE_BLUETOOTH_5:
		return "Bluetooth 5"
	case pb.DetectionSource_SOURCE_DJI_OCUSYNC:
		return "DJI DroneID"
	case pb.DetectionSource_SOURCE_ADS_B:
		return "ADS-B"
	case pb.DetectionSource_SOURCE_TSHARK_REMOTEID:
		return "tshark RemoteID"
	case pb.DetectionSource_SOURCE_TSHARK_SSID:
		return "tshark SSID"
	default:
		return "Unknown"
	}
}

// CommandAck represents a command acknowledgment from a TAP
type CommandAck struct {
	TapID      string
	CommandID  string
	Success    bool
	Error      string
	LatencyNs  int64
	ReceivedAt time.Time
}

// PendingCommand tracks commands awaiting acknowledgment
type PendingCommand struct {
	CommandID string
	TapID     string
	Command   string
	SentAt    time.Time
	AckCh     chan CommandAck // Optional channel to wait for ack
}

// NATSConfig holds NATS connection settings
type NATSConfig struct {
	URL string
}

// dbJob represents a database save job for the worker pool
type dbJob struct {
	drone *processor.Drone
	isNew bool
}

// PipelineStats holds atomic counters for pipeline health monitoring
type PipelineStats struct {
	DetectionsReceived  atomic.Int64 // Total detections received from NATS
	DetectionsProcessed atomic.Int64 // Successfully processed detections
	DBSaveSuccess       atomic.Int64 // Successful DB saves
	DBSaveErrors        atomic.Int64 // Failed DB saves
	DBQueueDrops        atomic.Int64 // Detections dropped due to full queue
	HeartbeatsReceived  atomic.Int64 // Total heartbeats received
}

// Receiver handles incoming NATS messages
type Receiver struct {
	nc          *nats.Conn
	state       *processor.StateManager
	spoof       *processor.SpoofDetector
	store       *storage.Store
	subs        []*nats.Subscription
	subsMu      sync.RWMutex // Protects subs slice (accessed by Start, Close, RefreshSubscriptions)
	rssiTracker *intel.RSSITracker
	calibrator  *intel.LiveCalibrator  // Live RSSI calibration from GPS ground truth
	kalman      *intel.KalmanRegistry  // Per-drone Kalman filters for trajectory smoothing

	// Worker pool for database saves (prevents goroutine explosion)
	dbQueue       chan dbJob
	dbWg          sync.WaitGroup
	dbCtx         context.Context
	dbCancel      context.CancelFunc
	dbWorkerCount int
	dbQueueSize   int

	// Pipeline health metrics
	Stats PipelineStats

	// Command tracking
	pendingCmds   map[string]*PendingCommand
	pendingCmdsMu sync.RWMutex

	// Detection dedup cache: prevents duplicate processing from at-least-once delivery
	dedupCache sync.Map // key: "tapID:mac:tsNS" -> int64(unixNano), cleaned by background ticker

	// TAP position cache: populated on heartbeat, avoids hitting state manager shard locks per detection
	tapCache sync.Map // key: tapID (string) -> *cachedTapPos

	// Semaphore for concurrent trilateration goroutines
	trilatSem chan struct{}
}

// ReceiverConfig holds optional tuning for the receiver
type ReceiverConfig struct {
	DBWorkerCount int `yaml:"db_worker_count"` // Number of parallel DB writers (default: 8)
	DBQueueSize   int `yaml:"db_queue_size"`   // DB job queue buffer size (default: 5000)
}

// New creates a new NATS receiver
func New(ctx context.Context, cfg NATSConfig, state *processor.StateManager, spoof *processor.SpoofDetector, store *storage.Store, rcfg ...ReceiverConfig) (*Receiver, error) {
	opts := []nats.Option{
		nats.Name("skylens-node"),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1), // Unlimited reconnects
		nats.PingInterval(10 * time.Second),    // Detect dead connections within 20s (was 4min default)
		nats.MaxPingsOutstanding(2),             // 2 missed pings = dead connection
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				slog.Warn("NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			slog.Info("NATS reconnected", "url", nc.ConnectedUrl())
			// Broadcast state refresh so WS clients know data is flowing again
			state.BroadcastRefresh()
		}),
	}

	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, err
	}

	slog.Info("Connected to NATS", "url", cfg.URL)

	// Apply receiver config
	workerCount := defaultDBWorkerCount
	queueSize := defaultDBQueueSize
	if len(rcfg) > 0 {
		if rcfg[0].DBWorkerCount > 0 {
			workerCount = rcfg[0].DBWorkerCount
		}
		if rcfg[0].DBQueueSize > 0 {
			queueSize = rcfg[0].DBQueueSize
		}
	}

	// Create context for worker pool
	dbCtx, dbCancel := context.WithCancel(context.Background())

	r := &Receiver{
		nc:            nc,
		state:         state,
		spoof:         spoof,
		store:         store,
		subs:          make([]*nats.Subscription, 0),
		rssiTracker:   intel.NewRSSITracker(50),
		calibrator:    intel.NewLiveCalibrator(0), // Uses default path loss exponent
		kalman:        intel.NewKalmanRegistry(),
		dbQueue:       make(chan dbJob, queueSize),
		dbCtx:         dbCtx,
		dbCancel:      dbCancel,
		dbWorkerCount: workerCount,
		dbQueueSize:   queueSize,
		pendingCmds:   make(map[string]*PendingCommand),
		trilatSem:     make(chan struct{}, 16), // Max 16 concurrent trilateration goroutines
	}

	// Start database worker pool (bounded goroutines)
	if store != nil {
		r.startDBWorkers()
	}

	return r, nil
}

// startDBWorkers launches the bounded worker pool for database saves
func (r *Receiver) startDBWorkers() {
	for i := 0; i < r.dbWorkerCount; i++ {
		r.dbWg.Add(1)
		go r.dbWorker(i)
	}
	slog.Info("Started database worker pool", "workers", r.dbWorkerCount, "queue_size", r.dbQueueSize)

	// Start queue depth monitor
	go r.monitorDBQueue()
}

// monitorDBQueue periodically logs queue depth and warns if getting full
func (r *Receiver) monitorDBQueue() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.dbCtx.Done():
			return
		case <-ticker.C:
			depth := len(r.dbQueue)
			pct := float64(depth) / float64(r.dbQueueSize) * 100

			if pct >= 80 {
				slog.Error("DB QUEUE CRITICAL - detections may be dropped",
					"depth", depth,
					"capacity", r.dbQueueSize,
					"percent_full", fmt.Sprintf("%.1f%%", pct),
					"db_errors", r.Stats.DBSaveErrors.Load(),
					"drops", r.Stats.DBQueueDrops.Load(),
				)
			} else if pct >= 50 {
				slog.Warn("DB queue filling up",
					"depth", depth,
					"capacity", r.dbQueueSize,
					"percent_full", fmt.Sprintf("%.1f%%", pct),
				)
			} else if depth > 0 {
				slog.Debug("DB queue status",
					"depth", depth,
					"capacity", r.dbQueueSize,
					"percent_full", fmt.Sprintf("%.1f%%", pct),
				)
			}
		}
	}
}

// dbWorker processes database save jobs from the queue in batches.
// Drains up to 10 jobs (with 5ms drain window) and executes them in a single pgx.Batch round-trip.
func (r *Receiver) dbWorker(id int) {
	defer r.dbWg.Done()

	const (
		maxBatchSize  = 10
		drainTimeout  = 5 * time.Millisecond
	)

	batch := make([]storage.DetectionJob, 0, maxBatchSize)
	drainTimer := time.NewTimer(drainTimeout)
	drainTimer.Stop()

	for {
		// Wait for first job
		select {
		case <-r.dbCtx.Done():
			slog.Debug("DB worker shutting down", "worker_id", id)
			return
		case job, ok := <-r.dbQueue:
			if !ok {
				return
			}
			batch = append(batch, storage.DetectionJob{Drone: job.drone, IsNew: job.isNew})
		}

		// Drain additional jobs up to maxBatchSize with a short timeout
		drainTimer.Reset(drainTimeout)
	drainLoop:
		for len(batch) < maxBatchSize {
			select {
			case job, ok := <-r.dbQueue:
				if !ok {
					break drainLoop
				}
				batch = append(batch, storage.DetectionJob{Drone: job.drone, IsNew: job.isNew})
			case <-drainTimer.C:
				break drainLoop
			}
		}
		// Stop timer if we filled the batch before it fired
		if !drainTimer.Stop() {
			select {
			case <-drainTimer.C:
			default:
			}
		}

		// Execute batch
		var err error
		for attempt := 0; attempt <= dbMaxRetries; attempt++ {
			err = r.store.SaveDetectionBatch(batch)
			if err == nil {
				r.Stats.DBSaveSuccess.Add(int64(len(batch)))
				break
			}
			if attempt < dbMaxRetries {
				time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
			}
		}
		if err != nil {
			r.Stats.DBSaveErrors.Add(int64(len(batch)))
			slog.Error("DB batch save failed after retries",
				"worker", id,
				"batch_size", len(batch),
				"error", err,
				"total_errors", r.Stats.DBSaveErrors.Load(),
			)
		}

		batch = batch[:0] // Reset for next round
	}
}

// GetPipelineStats returns current pipeline health metrics
func (r *Receiver) GetPipelineStats() map[string]interface{} {
	stats := map[string]interface{}{
		"db_queue_depth":       len(r.dbQueue),
		"db_queue_capacity":    r.dbQueueSize,
		"db_worker_count":      r.dbWorkerCount,
		"db_save_success":      r.Stats.DBSaveSuccess.Load(),
		"db_save_errors":       r.Stats.DBSaveErrors.Load(),
		"db_queue_drops":       r.Stats.DBQueueDrops.Load(),
		"detections_received":  r.Stats.DetectionsReceived.Load(),
		"detections_processed": r.Stats.DetectionsProcessed.Load(),
		"heartbeats_received":  r.Stats.HeartbeatsReceived.Load(),
	}
	if r.calibrator != nil {
		stats["rssi_calibration"] = r.calibrator.GetStats()
	}
	return stats
}

// GetCalibrator returns the live RSSI calibrator (for API access)
func (r *Receiver) GetCalibrator() *intel.LiveCalibrator {
	return r.calibrator
}

// FlushNATS forces a NATS PING/PONG exchange. If the connection is dead
// (e.g., after VM suspend/resume), the flush fails and triggers automatic
// reconnection. Returns error if the connection is unhealthy.
func (r *Receiver) FlushNATS(timeout time.Duration) error {
	return r.nc.FlushTimeout(timeout)
}

// RefreshSubscriptions tears down existing NATS subscriptions and creates fresh ones.
// This fixes stale subscription routing that can occur after system suspend/resume,
// where the TCP connection survives but NATS server stops delivering messages.
func (r *Receiver) RefreshSubscriptions() error {
	r.subsMu.Lock()

	// Unsubscribe all existing subscriptions
	for _, sub := range r.subs {
		if sub.IsValid() {
			if err := sub.Unsubscribe(); err != nil {
				slog.Debug("Error unsubscribing stale subscription", "subject", sub.Subject, "error", err)
			}
		}
	}

	// Create fresh subscriptions (same subjects/handlers as Start)
	var newSubs []*nats.Subscription

	detSub, err := r.nc.QueueSubscribe("skylens.detections.*", "skylens-nodes", r.handleDetection)
	if err != nil {
		r.subsMu.Unlock()
		return fmt.Errorf("failed to resubscribe detections: %w", err)
	}
	newSubs = append(newSubs, detSub)

	hbSub, err := r.nc.QueueSubscribe("skylens.heartbeats.*", "skylens-nodes", r.handleHeartbeat)
	if err != nil {
		// Cleanup the detection sub we just created
		detSub.Unsubscribe()
		r.subsMu.Unlock()
		return fmt.Errorf("failed to resubscribe heartbeats: %w", err)
	}
	newSubs = append(newSubs, hbSub)

	ackSub, err := r.nc.Subscribe("skylens.acks.*", r.handleCommandAck)
	if err != nil {
		// Cleanup subs we just created
		detSub.Unsubscribe()
		hbSub.Unsubscribe()
		r.subsMu.Unlock()
		return fmt.Errorf("failed to resubscribe acks: %w", err)
	}
	newSubs = append(newSubs, ackSub)

	r.subs = newSubs
	r.subsMu.Unlock()

	// Notify dashboard clients that data flow may resume
	r.state.BroadcastRefresh()

	slog.Info("NATS subscriptions refreshed")
	return nil
}

// Start begins receiving messages
func (r *Receiver) Start(ctx context.Context) error {
	// Subscribe to detections from all taps using queue group for load distribution
	// Multiple nodes will share the load instead of each receiving all messages
	detSub, err := r.nc.QueueSubscribe("skylens.detections.*", "skylens-nodes", r.handleDetection)
	if err != nil {
		return err
	}
	r.subsMu.Lock()
	r.subs = append(r.subs, detSub)
	r.subsMu.Unlock()
	slog.Info("Subscribed to skylens.detections.* (queue group: skylens-nodes)")

	// Subscribe to heartbeats from all taps using queue group
	hbSub, err := r.nc.QueueSubscribe("skylens.heartbeats.*", "skylens-nodes", r.handleHeartbeat)
	if err != nil {
		return err
	}
	r.subsMu.Lock()
	r.subs = append(r.subs, hbSub)
	r.subsMu.Unlock()
	slog.Info("Subscribed to skylens.heartbeats.* (queue group: skylens-nodes)")

	// Subscribe to command acknowledgments from all taps
	ackSub, err := r.nc.Subscribe("skylens.acks.*", r.handleCommandAck)
	if err != nil {
		return err
	}
	r.subsMu.Lock()
	r.subs = append(r.subs, ackSub)
	r.subsMu.Unlock()
	slog.Info("Subscribed to skylens.acks.*")

	// Start heartbeat watchdog: detects subscription staleness after suspend/resume
	// or NATS routing glitches where TCP stays alive but messages stop flowing.
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("PANIC in heartbeat watchdog goroutine", "panic", rec)
			}
		}()

		const checkInterval = 30 * time.Second
		const silenceThreshold = 90 * time.Second
		const maxConsecRefresh = 3
		const backoffInterval = 5 * time.Minute

		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()

		lastHBCount := r.Stats.HeartbeatsReceived.Load()
		lastHBChangeTime := time.Now()
		hadTaps := false
		consecRefreshFails := 0

		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				currentHBCount := r.Stats.HeartbeatsReceived.Load()

				if currentHBCount > lastHBCount {
					// Heartbeats are flowing — reset everything
					lastHBCount = currentHBCount
					lastHBChangeTime = now
					hadTaps = true
					consecRefreshFails = 0
					continue
				}

				// No new heartbeats since last check
				if !hadTaps {
					// Never received heartbeats — nothing to recover
					lastHBCount = currentHBCount
					continue
				}

				silenceDuration := now.Sub(lastHBChangeTime)
				if silenceDuration < silenceThreshold {
					continue
				}

				// Heartbeat silence detected
				if consecRefreshFails >= maxConsecRefresh {
					slog.Error("Heartbeat silence persists after multiple refresh attempts, backing off",
						"silence_duration", silenceDuration.Round(time.Second),
						"consecutive_failures", consecRefreshFails,
						"next_check_in", backoffInterval,
					)
					// Back off: sleep until next backoff interval, then reset counter to try again
					select {
					case <-ctx.Done():
						return
					case <-time.After(backoffInterval - checkInterval):
					}
					consecRefreshFails = 0
					lastHBChangeTime = time.Now()
					continue
				}

				slog.Warn("Heartbeat silence detected, refreshing NATS subscriptions",
					"silence_duration", silenceDuration.Round(time.Second),
					"last_hb_count", lastHBCount,
					"attempt", consecRefreshFails+1,
				)

				if err := r.RefreshSubscriptions(); err != nil {
					slog.Error("Subscription refresh failed", "error", err)
				}

				consecRefreshFails++
				lastHBChangeTime = time.Now() // Reset timer to avoid rapid-fire refreshes
				lastHBCount = r.Stats.HeartbeatsReceived.Load()
			}
		}
	}()

	// Start periodic calibration pruning and pendingCmds cleanup
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("PANIC in calibration/cleanup goroutine", "panic", rec)
			}
		}()
		calibTicker := time.NewTicker(30 * time.Minute)
		cmdTicker := time.NewTicker(5 * time.Minute)
		dedupTicker := time.NewTicker(5 * time.Second)
		defer calibTicker.Stop()
		defer cmdTicker.Stop()
		defer dedupTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-dedupTicker.C:
				// Evict stale dedup entries (moved off hot path)
				cutoff := time.Now().Add(-5 * time.Second).UnixNano()
				r.dedupCache.Range(func(key, val any) bool {
					if ts, ok := val.(int64); ok && ts < cutoff {
						r.dedupCache.Delete(key)
					}
					return true
				})
			case <-calibTicker.C:
				if r.calibrator != nil {
					r.calibrator.PruneStale()
					slog.Debug("Pruned stale calibration data")
				}
			case <-cmdTicker.C:
				// Evict pending commands older than 2 minutes (unacknowledged)
				cutoff := time.Now().Add(-2 * time.Minute)
				r.pendingCmdsMu.Lock()
				for id, cmd := range r.pendingCmds {
					if cmd.SentAt.Before(cutoff) {
						delete(r.pendingCmds, id)
					}
				}
				r.pendingCmdsMu.Unlock()
			}
		}
	}()

	// Wait for context cancellation
	<-ctx.Done()
	return nil
}

// handleDetection processes incoming drone detections (Protobuf)
func (r *Receiver) handleDetection(msg *nats.Msg) {
	r.Stats.DetectionsReceived.Add(1)

	var det pb.Detection
	if err := proto.Unmarshal(msg.Data, &det); err != nil {
		slog.Warn("Failed to parse detection", "error", err)
		return
	}

	// Dedup: skip if we've seen this exact detection recently (at-least-once delivery protection)
	dedupKey := det.TapId + ":" + det.MacAddress + ":" + strconv.FormatInt(det.TimestampNs, 10)
	now := time.Now()
	if _, loaded := r.dedupCache.LoadOrStore(dedupKey, now.UnixNano()); loaded {
		return // Already processed
	}
	// Dedup cleanup runs on a background ticker (see Start), not here on the hot path

	// Check if this is a suspect candidate (WiFi detection without RemoteID)
	// Route suspects through correlator before promoting to full drone tracking
	if det.Manufacturer == "UNKNOWN" && det.Designation == "SUSPECT" {
		r.handleSuspectDetection(&det)
		return
	}

	// === VALIDATE CONTROLLER FLAG ===
	// Don't blindly trust TAP's is_controller flag — verify SSID matches a known controller pattern.
	// Prevents false positives from misconfigured TAPs or non-drone WiFi devices.
	if det.IsController {
		ssidMatch := intel.MatchSSID(det.Ssid)
		if ssidMatch == nil || !ssidMatch.IsController {
			slog.Warn("Rejected is_controller=true: SSID doesn't match known controller pattern",
				"ssid", det.Ssid,
				"mac", det.MacAddress,
				"tap", det.TapId,
			)
			det.IsController = false // Downgrade — let it go through normal beacon filter below
		}
	}

	// === REJECT NON-DRONE WIFI BEACONS ===
	// WIFI_BEACON detections without RemoteID payload and without drone OUI/SSID
	// are almost certainly regular WiFi access points, not drones.
	// Exception: TAP-flagged controllers (is_controller=true) always pass through.
	if det.Source == pb.DetectionSource_SOURCE_WIFI_BEACON && len(det.RemoteidPayload) == 0 && !det.IsController {
		hasDroneOUI := intel.MatchOUI(det.MacAddress) != ""
		hasDroneSSID := det.Ssid != "" && intel.MatchSSID(det.Ssid) != nil
		hasSerialNumber := det.SerialNumber != ""

		if !hasDroneOUI && !hasDroneSSID && !hasSerialNumber {
			slog.Debug("Rejected WIFI_BEACON without drone indicators",
				"mac", det.MacAddress,
				"ssid", det.Ssid,
				"identifier", det.Identifier,
			)
			return
		}
	}

	// Parse RemoteID payload if present for enhanced telemetry
	var parsedRemoteID *intel.ParsedRemoteID
	var parsedDJI *intel.DJIDroneID
	if len(det.RemoteidPayload) > 0 {
		slog.Debug("RemoteID/DJI payload present",
			"identifier", det.Identifier,
			"payload_len", len(det.RemoteidPayload),
			"source", det.Source.String(),
		)

		// Try ASTM F3411 RemoteID first
		if rid, err := intel.ParseRemoteIDPayload(det.RemoteidPayload); err == nil && rid != nil {
			parsedRemoteID = rid
			slog.Debug("Parsed ASTM F3411 RemoteID",
				"identifier", det.Identifier,
				"serial", rid.SerialNumber,
				"operator_id", rid.OperatorID,
				"ua_type", rid.UAType.String(),
				"messages", len(rid.MessageTypes),
			)
		} else if err != nil {
			slog.Debug("ASTM RemoteID parse failed",
				"identifier", det.Identifier,
				"error", err,
			)
		}

		// Also try DJI DroneID (uses different OUI)
		if len(det.RemoteidPayload) >= 3 {
			if intel.IsDJIOUI(det.RemoteidPayload[:3]) {
				// DJI vendor IE: OUI(3) + type(1) + payload
				if len(det.RemoteidPayload) >= 4 {
					subCmd := det.RemoteidPayload[3]
					dji, err := intel.ParseDJIDroneID(det.RemoteidPayload[4:], subCmd)
					if err == nil && dji != nil {
						parsedDJI = dji
						slog.Info("Parsed DJI DroneID successfully",
							"identifier", det.Identifier,
							"serial", dji.SerialNumber,
							"model", dji.ProductModel,
							"in_air", dji.InAir,
							"motors", dji.MotorsOn,
							"lat", dji.Latitude,
							"lon", dji.Longitude,
						)
					} else {
						slog.Warn("DJI DroneID OUI matched but parse FAILED",
							"identifier", det.Identifier,
							"oui", fmt.Sprintf("%02X:%02X:%02X", det.RemoteidPayload[0], det.RemoteidPayload[1], det.RemoteidPayload[2]),
							"subCmd", fmt.Sprintf("0x%02X", subCmd),
							"payload_len", len(det.RemoteidPayload)-4,
							"error", err,
						)
					}
				} else {
					slog.Warn("DJI OUI matched but payload too short",
						"identifier", det.Identifier,
						"payload_len", len(det.RemoteidPayload),
						"need", 4,
					)
				}
			} else {
				// Log non-DJI OUI for debugging
				slog.Debug("RemoteID payload OUI is not DJI",
					"identifier", det.Identifier,
					"payload_len", len(det.RemoteidPayload),
				)
			}
		}
	} else {
		// No RemoteID payload - this is a behavioral/WiFi-only detection
		slog.Debug("No RemoteID payload",
			"identifier", det.Identifier,
			"source", det.Source.String(),
		)
	}

	// Log full detection for debugging (Debug level to avoid 50 lines/sec at scale)
	slog.Debug("Detection received",
		"tap_id", det.TapId,
		"identifier", det.Identifier,
		"mac", det.MacAddress,
		"ssid", det.Ssid,
		"serial", det.SerialNumber,
		"lat", det.Latitude,
		"lon", det.Longitude,
		"alt_geo", det.AltitudeGeodetic,
		"alt_press", det.AltitudePressure,
		"height_agl", det.HeightAgl,
		"speed", det.Speed,
		"heading", det.Heading,
		"track_dir", det.TrackDirection,
		"op_lat", det.OperatorLatitude,
		"op_lon", det.OperatorLongitude,
		"op_alt", det.OperatorAltitude,
		"rssi", det.Rssi,
		"channel", det.Channel,
		"beacon_interval", det.BeaconIntervalTu,
		"source", det.Source.String(),
		"designation", det.Designation,
		"manufacturer", det.Manufacturer,
		"is_controller", det.IsController,
	)

	// Validate coordinates - should be WGS84 degrees from tap
	// If coordinates look like raw ASTM F3411 integers, log warning
	if det.Latitude != 0 && (det.Latitude > 900000000 || det.Latitude < -900000000) {
		slog.Warn("Latitude looks like raw ASTM F3411 (needs *1e-7 scaling on tap side)",
			"raw_lat", det.Latitude,
			"scaled", det.Latitude*1e-7,
		)
	}
	if det.Longitude != 0 && (det.Longitude > 1800000000 || det.Longitude < -1800000000) {
		slog.Warn("Longitude looks like raw ASTM F3411 (needs *1e-7 scaling on tap side)",
			"raw_lon", det.Longitude,
			"scaled", det.Longitude*1e-7,
		)
	}

	// Use TrackDirection if available, otherwise fall back to Heading
	groundTrack := det.TrackDirection
	if groundTrack == 0 && det.Heading != 0 {
		groundTrack = det.Heading
	}

	// Model identification - check DJI parser first, then serial lookup
	model := ""
	if parsedDJI != nil && parsedDJI.ProductModel != "" {
		model = parsedDJI.ProductModel
	}
	if model == "" {
		model = intel.GetModelFromSerial(det.SerialNumber)
	}
	if model == "" && det.Designation != "" {
		model = det.Designation
	}

	// Early identity recall for dark drones (no RemoteID model info).
	// Must happen BEFORE range ring creation so we use model-calibrated RSSI_0
	// instead of generic fallback. Uses identifier/MAC to recall previously seen model.
	if model == "" && det.Rssi != 0 {
		if det.Identifier != "" {
			if recalledModel, _, ok := r.calibrator.RecallModel(det.Identifier); ok {
				model = recalledModel
				slog.Debug("Recalled drone model from identity memory (early)",
					"identifier", det.Identifier,
					"model", model,
				)
			}
		}
		if model == "" && det.MacAddress != "" {
			if recalledModel, _, ok := r.calibrator.RecallModel(det.MacAddress); ok {
				model = recalledModel
				slog.Debug("Recalled drone model from MAC memory (early)",
					"mac", det.MacAddress,
					"model", model,
				)
			}
		}
	}

	// RSSI tracking and distance estimation
	// Gate BLE detections out of WiFi-based distance estimation.
	// BLE uses different TX power (+4 dBm vs +17-23 dBm WiFi) and different
	// radios/antennas. Applying the WiFi propagation model to BLE RSSI produces
	// 10-46x distance overestimates. BLE detections still go through for
	// identity/classification/state tracking, just not distance estimation.
	isBLE := det.Source == pb.DetectionSource_SOURCE_BLUETOOTH_4 ||
		det.Source == pb.DetectionSource_SOURCE_BLUETOOTH_5

	var rssiAnalysis intel.RSSIAnalysis
	var distanceEst float64
	var rangeRing *processor.RangeRing
	if det.Rssi != 0 && !isBLE {
		rssiAnalysis = r.rssiTracker.Track(det.Identifier, float64(det.Rssi), model)
		distanceEst = rssiAnalysis.DistanceEstM

		// Log approach/departure events
		if rssiAnalysis.Movement == intel.MovementApproaching {
			slog.Info("APPROACH detected",
				"identifier", det.Identifier,
				"rssi", det.Rssi,
				"trend", rssiAnalysis.Trend,
				"distance_est", distanceEst,
			)
		} else if rssiAnalysis.Movement == intel.MovementDeparting {
			slog.Debug("Departure detected",
				"identifier", det.Identifier,
				"rssi", det.Rssi,
				"trend", rssiAnalysis.Trend,
			)
		}

		// Create range ring using live calibration (falls back to static offsets)
		if tapLat, tapLon, tapOk := r.getTapPos(det.TapId); tapOk && tapLat != 0 && tapLon != 0 {
			liveBounds := r.calibrator.EstimateDistanceLiveWithBounds(float64(det.Rssi), model, det.TapId)
			if liveBounds.DistanceM > 0 {
				rangeRing = &processor.RangeRing{
					TapID:       det.TapId,
					TapLat:      tapLat,
					TapLon:      tapLon,
					DistanceM:   liveBounds.DistanceM,
					MinM:        liveBounds.DistanceMinM,
					MaxM:        liveBounds.DistanceMaxM,
					Confidence:  liveBounds.Confidence,
					RSSI:        det.Rssi,
					Environment: liveBounds.Environment,
				}
				distanceEst = liveBounds.DistanceM
				slog.Debug("Range ring created",
					"tap", det.TapId, "distance_m", liveBounds.DistanceM,
					"model", model, "rssi", det.Rssi,
				)
			}
		} else {
			slog.Debug("Range ring skipped - no tap position",
				"tap_id", det.TapId, "tap_found", tapOk,
			)
		}
	}

	// If model not identified from serial, try from designation or SSID via fingerprinting
	if model == "" && det.Designation != "" {
		if ssidMatch := intel.MatchSSID(det.Designation); ssidMatch != nil {
			if ssidMatch.Model != "" {
				model = ssidMatch.Model
			} else if ssidMatch.ModelHint != "" {
				model = ssidMatch.Manufacturer + " " + ssidMatch.ModelHint
			}
		}
	}
	// Also try SSID if model still not identified
	if model == "" && det.Ssid != "" {
		if ssidMatch := intel.MatchSSID(det.Ssid); ssidMatch != nil {
			if ssidMatch.Model != "" {
				model = ssidMatch.Model
			} else if ssidMatch.ModelHint != "" {
				model = ssidMatch.Manufacturer + " " + ssidMatch.ModelHint
			}
		}
	}

	// Set designation - prefer meaningful name over generic "WiFi RemoteID broadcast"
	designation := det.Designation
	// Will be overridden after RemoteID parsing if we get better info

	// === Enrich detection with parsed RemoteID/DJI data ===
	// Start with protobuf values, override with parsed data where better
	latitude := det.Latitude
	longitude := det.Longitude
	altGeo := det.AltitudeGeodetic
	altPress := det.AltitudePressure
	heightAGL := det.HeightAgl
	speed := det.Speed
	vSpeed := det.VerticalSpeed
	heading := det.Heading
	opLat := det.OperatorLatitude
	opLon := det.OperatorLongitude
	opAlt := det.OperatorAltitude
	operatorID := det.OperatorId
	serial := det.SerialNumber
	uavType := det.UavType
	manufacturer := det.Manufacturer

	// Extract serial from SSID if it starts with "RID-" (WiFi RemoteID broadcast)
	// The SSID format is: RID-<serial_number>
	if strings.HasPrefix(det.Ssid, "RID-") && len(det.Ssid) > 4 {
		ssidSerial := det.Ssid[4:] // Extract everything after "RID-"
		// Only use if current serial is empty or contains garbage
		if serial == "" || !isCleanSerial(serial) {
			serial = ssidSerial
			slog.Debug("Extracted serial from SSID",
				"ssid", det.Ssid,
				"serial", serial,
			)
		}
	}

	// Enrich from parsed ASTM F3411 RemoteID
	if parsedRemoteID != nil {
		if parsedRemoteID.HasValidPosition() {
			latitude = parsedRemoteID.Latitude
			longitude = parsedRemoteID.Longitude
		}
		if parsedRemoteID.AltitudeGeo != 0 {
			altGeo = parsedRemoteID.AltitudeGeo
		}
		if parsedRemoteID.AltitudeBaro != 0 {
			altPress = parsedRemoteID.AltitudeBaro
		}
		if parsedRemoteID.HeightAGL != 0 {
			heightAGL = parsedRemoteID.HeightAGL
		}
		if parsedRemoteID.Speed != 0 {
			speed = parsedRemoteID.Speed
		}
		if parsedRemoteID.VerticalSpeed != 0 {
			vSpeed = parsedRemoteID.VerticalSpeed
		}
		if parsedRemoteID.TrackDirection != 0 {
			heading = parsedRemoteID.TrackDirection
		}
		if parsedRemoteID.HasOperatorPosition() {
			opLat = parsedRemoteID.OperatorLat
			opLon = parsedRemoteID.OperatorLon
			opAlt = parsedRemoteID.OperatorAlt
		}
		if parsedRemoteID.SerialNumber != "" {
			serial = parsedRemoteID.SerialNumber
		}
		if parsedRemoteID.OperatorID != "" {
			operatorID = parsedRemoteID.OperatorID
		}
		if parsedRemoteID.UAType != 0 {
			uavType = parsedRemoteID.UAType.String()
		}
	}

	// Enrich from parsed DJI DroneID (DJI-specific data takes priority)
	if parsedDJI != nil {
		if parsedDJI.HasValidPosition() {
			latitude = parsedDJI.Latitude
			longitude = parsedDJI.Longitude
		}
		if parsedDJI.Altitude != 0 {
			altPress = parsedDJI.Altitude
		}
		if parsedDJI.Height != 0 {
			heightAGL = parsedDJI.Height
		}
		if parsedDJI.Speed != 0 {
			speed = parsedDJI.Speed
		}
		if parsedDJI.GetVerticalSpeed() != 0 {
			vSpeed = parsedDJI.GetVerticalSpeed()
		}
		if parsedDJI.Yaw != 0 {
			heading = parsedDJI.Yaw
		}
		if parsedDJI.HasPilotPosition() {
			opLat = parsedDJI.PilotLat
			opLon = parsedDJI.PilotLon
		} else if parsedDJI.HasHomePosition() {
			// DJI home/takeoff location is typically near the pilot
			opLat = parsedDJI.HomeLat
			opLon = parsedDJI.HomeLon
		}
		if parsedDJI.SerialNumber != "" {
			serial = parsedDJI.SerialNumber
		}
		if parsedDJI.ProductModel != "" {
			model = parsedDJI.ProductModel
			designation = parsedDJI.ProductModel
		}
		manufacturer = "DJI"
	}

	// Decode manufacturer/model from serial number
	if serial != "" {
		// Check for DJI CTA-2063-A serial format (1581Fxxxx...)
		if strings.HasPrefix(serial, "1581F") {
			manufacturer = "DJI"
			decodedModel := intel.DecodeModelFromSerial(serial)
			if decodedModel != "" {
				// Serial decoder is authoritative for DJI — always override TAP's model
				model = decodedModel
			}
		} else if manufacturer == "" || manufacturer == "UNKNOWN" || manufacturer == "RemoteID" {
			// Non-DJI serial: only set if manufacturer is unknown
			manufacturer = "UNKNOWN"
		}
	}

	// Build meaningful designation from parsed data
	if isGenericDesignation(designation) {
		if model != "" {
			designation = model
		} else if manufacturer != "" && manufacturer != "UNKNOWN" {
			if serial != "" {
				// Use manufacturer + short serial
				shortSerial := serial
				if len(shortSerial) > 12 {
					shortSerial = shortSerial[:12]
				}
				designation = manufacturer + " " + shortSerial
			} else {
				designation = manufacturer + " UAV"
			}
		} else if serial != "" {
			// Use serial number
			shortSerial := serial
			if len(shortSerial) > 16 {
				shortSerial = shortSerial[:16]
			}
			designation = "UAV " + shortSerial
		} else if uavType != "" && uavType != "None" {
			designation = uavType
		}
	}

	// Generate clean identifier - TAP may send garbage in identifier field
	// Priority: serial_number > MAC address > original identifier
	cleanIdentifier := det.Identifier
	if !isValidIdentifier(cleanIdentifier) {
		// Identifier has garbage, use serial or MAC instead
		if serial != "" {
			cleanIdentifier = serial
			slog.Debug("Using serial as identifier (original had garbage)",
				"serial", serial,
				"original_len", len(det.Identifier),
			)
		} else if det.MacAddress != "" {
			cleanIdentifier = det.MacAddress
			slog.Debug("Using MAC as identifier (original had garbage)",
				"mac", det.MacAddress,
				"original_len", len(det.Identifier),
			)
		}
	}
	if cleanIdentifier == "" {
		slog.Warn("Dropping detection with no usable identifier",
			"tap", det.TapId, "mac", det.MacAddress)
		return
	}

	// Validate and clean serial number - TAP sometimes sends garbage
	cleanSerial := serial
	if serial != "" && !isCleanSerial(serial) {
		// Try to extract valid portion if it looks like it has garbage prefix
		// e.g., "\x01\x02RID-1581F7FVC251A00CB04F" -> "1581F7FVC251A00CB04F"
		cleanSerial = ""
		for i := 0; i < len(serial); i++ {
			if serial[i] >= '0' && serial[i] <= '9' {
				// Found start of numeric portion, check if it's a valid DJI serial
				remaining := serial[i:]
				if strings.HasPrefix(remaining, "1581F") || strings.HasPrefix(remaining, "1582F") ||
					strings.HasPrefix(remaining, "1583F") || strings.HasPrefix(remaining, "1584F") {
					// Found DJI serial prefix, extract until garbage
					for j := 0; j < len(remaining); j++ {
						if remaining[j] < 0x20 || remaining[j] > 0x7E {
							cleanSerial = remaining[:j]
							break
						}
						if j == len(remaining)-1 {
							cleanSerial = remaining
						}
					}
					break
				}
			}
		}
		if cleanSerial != "" {
			slog.Debug("Cleaned garbage from serial number",
				"original_len", len(serial),
				"clean", cleanSerial,
			)
		}
	}

	// Validate drone GPS coordinates — WGS84 hard bounds + proximity to detecting TAP
	if latitude != 0 || longitude != 0 {
		if math.Abs(latitude) > 90 || math.Abs(longitude) > 180 {
			slog.Warn("GPS coordinates out of WGS84 bounds, discarding",
				"identifier", det.Identifier,
				"lat", latitude, "lon", longitude,
			)
			latitude = 0
			longitude = 0
		} else if tapLat, tapLon, tapOk := r.getTapPos(det.TapId); tapOk && tapLat != 0 {
			distKm := haversineKmSimple(tapLat, tapLon, latitude, longitude)
			if distKm > 50 {
				slog.Warn("GPS coordinates too far from detecting TAP, discarding",
					"identifier", det.Identifier,
					"tap", det.TapId,
					"drone_lat", latitude, "drone_lon", longitude,
					"tap_lat", tapLat, "tap_lon", tapLon,
					"distance_km", distKm,
				)
				latitude = 0
				longitude = 0
			}
		}
	}
	if opLat != 0 || opLon != 0 {
		if math.Abs(opLat) > 90 || math.Abs(opLon) > 180 {
			slog.Warn("Operator coordinates out of WGS84 bounds, discarding",
				"identifier", det.Identifier,
				"op_lat", opLat, "op_lon", opLon,
			)
			opLat = 0
			opLon = 0
			opAlt = 0
		} else if tapLat, tapLon, tapOk := r.getTapPos(det.TapId); tapOk && tapLat != 0 {
			distKm := haversineKmSimple(tapLat, tapLon, opLat, opLon)
			if distKm > 50 {
				slog.Warn("Operator coordinates too far from detecting TAP, discarding",
					"identifier", det.Identifier,
					"tap", det.TapId,
					"op_lat", opLat, "op_lon", opLon,
					"distance_km", distKm,
				)
				opLat = 0
				opLon = 0
				opAlt = 0
			}
		}
	}

	// Validate operator location relative to drone
	if opLat != 0 && opLon != 0 && latitude != 0 && longitude != 0 {
		opDistKm := haversineKmSimple(latitude, longitude, opLat, opLon)
		if opDistKm > 50 || opAlt > 10000 {
			// Unrealistic (>50km or >10km alt) — discard instead of copying drone position
			slog.Warn("Operator location unrealistic, discarding",
				"drone_lat", latitude, "drone_lon", longitude,
				"op_lat", opLat, "op_lon", opLon,
				"op_alt", opAlt, "distance_km", opDistKm,
			)
			opLat = 0
			opLon = 0
			opAlt = 0
		} else if opDistKm < 0.001 {
			// Operator position within 1m of drone — almost certainly the drone
			// echoing its own GPS as operator location (buggy RemoteID broadcast).
			// Real pilots at takeoff are typically 3-10m away.
			opLat = 0
			opLon = 0
			opAlt = 0
		}
	} else if (latitude == 0 && longitude == 0) && opLat != 0 && opLon != 0 && det.IsController {
		// Controller with no drone GPS — operator coords are TAP GPS, not real operator location.
		// Zero them out so no misleading operator pin shows on the map.
		// Trilateration from multi-TAP RSSI will compute the actual position.
		opLat = 0
		opLon = 0
		opAlt = 0
	}

	// Convert protobuf to Drone struct with enriched data
	drone := &processor.Drone{
		Identifier:           cleanIdentifier,
		MACAddress:           det.MacAddress,
		SerialNumber:         cleanSerial,
		Registration:         det.Registration,
		SessionID:            det.SessionId,
		UTMID:                det.UtmId,
		Latitude:             latitude,
		Longitude:            longitude,
		AltitudeGeodetic:     altGeo,
		AltitudePressure:     altPress,
		HeightAGL:            heightAGL,
		HeightReference:      det.HeightReference.String(),
		Speed:                speed,
		VerticalSpeed:        vSpeed,
		Heading:              heading,
		GroundTrack:          groundTrack,
		OperatorLatitude:     opLat,
		OperatorLongitude:    opLon,
		OperatorAltitude:     opAlt,
		OperatorID:           operatorID,
		OperatorLocationType: det.OperatorLocationType.String(),
		RSSI:                 det.Rssi,
		Channel:              det.Channel,
		FrequencyMHz:         det.FrequencyMhz,
		SSID:                 det.Ssid,
		BeaconIntervalTU:     det.BeaconIntervalTu,
		CountryCode:          det.CountryCode,
		HTCapabilities:       det.HtCapabilities,
		DetectionSource:      detectionSourceDisplayName(det.Source),
		UAVType:              uavType,
		UAVCategory:          det.UavCategory.String(),
		OperationalStatus:    normalizeOpStatus(det.OperationalStatus.String()),
		Confidence:           det.Confidence,
		IsController:         det.IsController,
		Designation:          designation,
		Manufacturer:         manufacturer,
		Model:                model,
		TapID:                det.TapId,
		RawFrame:             det.RawFrame,
		RemoteIDPayload:      det.RemoteidPayload,
	}

	// === Live RSSI Calibration ===
	// If we have both GPS and RSSI, feed the calibrator with a ground-truth pair.
	// This teaches the system the actual TX power of this drone model at this tap.
	// Identity recall was already done earlier (before range ring creation).
	// BLE detections are excluded: different radio/antenna/TX power contaminates WiFi calibration.
	if model != "" && det.Rssi != 0 && latitude != 0 && longitude != 0 && !isBLE {
		// Remember this identifier's model for when it goes dark
		r.calibrator.RememberIdentity(cleanIdentifier, model, cleanSerial)
		if det.MacAddress != "" {
			r.calibrator.RememberIdentity(det.MacAddress, model, cleanSerial)
		}

		// Compute actual distance from TAP to drone using GPS
		if tapLat, tapLon, tapOk := r.getTapPos(det.TapId); tapOk && tapLat != 0 && tapLon != 0 {
			actualDist := intel.HaversineDistance(tapLat, tapLon, latitude, longitude)
			r.calibrator.RecordCalibrationPoint(model, det.TapId, float64(det.Rssi), actualDist)
		}
	}

	// === Controller RSSI Calibration via Linked UAV Operator Position ===
	// If this UAV has a linked controller AND operator position (which IS the pilot/controller location),
	// use the operator position as ground truth for the controller's RSSI-distance model.
	if opLat != 0 && opLon != 0 && !drone.IsController {
		if link := r.state.GetControllerLink(cleanIdentifier); link != nil {
			if ctrl, ok := r.state.GetDrone(link.ControllerID); ok && ctrl.RSSI < 0 && ctrl.Model != "" {
				if ctrlTapLat, ctrlTapLon, ctrlTapOk := r.getTapPos(ctrl.TapID); ctrlTapOk && ctrlTapLat != 0 && ctrlTapLon != 0 {
					ctrlDist := intel.HaversineDistance(ctrlTapLat, ctrlTapLon, opLat, opLon)
					if ctrlDist > 50 && ctrlDist < 15000 {
						r.calibrator.RecordCalibrationPoint(ctrl.Model, ctrl.TapID, float64(ctrl.RSSI), ctrlDist)
					}
				}
			}
		}
	}

	// Check if this drone matches a previously tracked suspect candidate
	// This allows merging historical observations when a drone later broadcasts RemoteID
	if upgradedCandidate := r.state.CheckRemoteIDUpgrade(drone); upgradedCandidate != nil {
		// Merge suspect candidate data into the drone
		// Add observations from suspect tracking period
		drone.DetectionCount = int64(upgradedCandidate.Observations)
		// If we didn't have a good first_seen, use the suspect's first_seen
		if drone.FirstSeen.IsZero() || upgradedCandidate.FirstSeen.Before(drone.FirstSeen) {
			drone.FirstSeen = upgradedCandidate.FirstSeen
		}
		slog.Info("Drone matched to prior suspect candidate",
			"identifier", drone.Identifier,
			"mac", drone.MACAddress,
			"prior_observations", upgradedCandidate.Observations,
			"prior_taps", len(upgradedCandidate.TapsSeen),
		)
	}

	// Run spoof detection
	trustScore, flags := r.spoof.Analyze(drone)
	drone.TrustScore = trustScore
	drone.SpoofFlags = flags

	// Classify based on trust score
	drone.Classification = classifyByTrust(trustScore, flags)

	// Update state - resolvedID is the actual identifier the drone is stored under
	// (may differ from drone.Identifier if merged via serial/MAC lookup)
	isNew, resolvedID := r.state.UpdateDrone(drone)
	r.Stats.DetectionsProcessed.Add(1)

	// Sync identifier for DB write: when a drone is resolved via serial/MAC dedup,
	// the DB row uses the original identifier, not the new detection's identifier.
	// Without this, UPDATE WHERE identifier=$1 matches zero rows and data is silently lost.
	if resolvedID != "" && resolvedID != drone.Identifier {
		slog.Debug("Using resolved identifier for DB write",
			"detection_id", drone.Identifier,
			"resolved_id", resolvedID,
		)
		drone.Identifier = resolvedID
	}

	// Store range ring ALWAYS (data must not be lost), then run trilateration
	// off the hot path with bounded concurrency. Previously both ring storage
	// AND trilateration were inside the semaphore gate, causing data loss.
	if rangeRing != nil {
		rr := *rangeRing
		rid := resolvedID
		// Always store the range ring — this is just a map upsert, very fast
		r.state.UpdateDroneRangeRing(rid, rr)
		// Run trilateration in bounded goroutine
		select {
		case r.trilatSem <- struct{}{}:
			go func() {
				defer func() { <-r.trilatSem }()
				r.runTrilateration(rid)
			}()
		default:
			// All slots busy — skip trilateration for this detection (next one will catch up)
			// Ring is already stored, so next trilateration will use it
		}
	}

	// Queue for database storage (bounded worker pool prevents goroutine explosion)
	if r.store != nil && r.dbQueue != nil {
		select {
		case r.dbQueue <- dbJob{drone: drone, isNew: isNew}:
			// Queued successfully
		default:
			r.Stats.DBQueueDrops.Add(1)
			slog.Warn("DB queue full, dropping detection",
				"identifier", drone.Identifier,
				"total_drops", r.Stats.DBQueueDrops.Load(),
			)
		}
	}
}

// runTrilateration runs position estimation from stored range rings.
// Range ring storage is done separately (always succeeds); this only handles
// the compute-heavy trilateration which can be skipped under load.
func (r *Receiver) runTrilateration(resolvedID string) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("PANIC in trilateration goroutine", "panic", rec, "drone", resolvedID,
				"stack", string(debugStack()))
		}
	}()

	// Get all range rings and filter stale ones (>30s old)
	allRings := r.state.GetDroneRangeRings(resolvedID)
	now := time.Now()
	rings := make([]processor.RangeRing, 0, len(allRings))
	for _, ring := range allRings {
		if ring.UpdatedAt.IsZero() || now.Sub(ring.UpdatedAt) > 30*time.Second {
			continue // Skip stale or uninitialized ring
		}
		rings = append(rings, ring)
	}

	if len(rings) == 0 && len(allRings) > 0 {
		// All rings went stale — clear the estimated position and Kalman filter
		// so the map doesn't show a phantom marker at the last known location.
		r.state.SetDroneEstimatedPosition(resolvedID, nil)
		r.kalman.Remove(resolvedID)
		r.state.BroadcastDroneUpdate(resolvedID)
		return
	}

	if len(rings) >= 2 {
		// Convert range rings to TapDistance for trilateration
		tapDistances := make([]intel.TapDistance, len(rings))
		for i, ring := range rings {
			tapDistances[i] = intel.TapDistance{
				TapID:        ring.TapID,
				Latitude:     ring.TapLat,
				Longitude:    ring.TapLon,
				DistanceM:    ring.DistanceM,
				UncertaintyM: (ring.MaxM - ring.MinM) / 2,
				RSSI:         float64(ring.RSSI),
				Confidence:   ring.Confidence,
			}
		}

		// Trilateration requires 3+ unique TAP positions; skip for 2 to avoid
		// guaranteed failure + wasted computation (goes straight to circle intersection)
		var result *intel.TrilaterationResult
		var err error
		if len(tapDistances) >= 3 {
			result, err = intel.Trilaterate(tapDistances, intel.DefaultTrilaterationConfig())
			if err != nil {
				slog.Debug("Trilateration failed", "error", err, "rings", len(rings))
			}
		}
		if err == nil && result != nil {
			estPos := &processor.EstimatedPosition{
				Latitude:   result.Latitude,
				Longitude:  result.Longitude,
				ErrorM:     result.ErrorM,
				Confidence: result.Confidence,
				TapsUsed:   result.TapsUsed,
				Method:     "weighted_least_squares",
				SemiMajorM: result.SemiMajorM,
				SemiMinorM: result.SemiMinorM,
				Timestamp:  time.Now(),
			}
			r.setFilteredPosition(resolvedID, estPos)
		} else if len(rings) >= 2 {
			// Fallback: deduplicate co-located TAPs and use range-ratio interpolation.
			// Group rings by unique position (merge co-located TAPs).
			type uniqueTap struct {
				lat, lon, dist, unc float64
				count               int
			}
			tapMap := map[string]*uniqueTap{}
			for _, td := range tapDistances {
				// Round to ~11m grid to merge co-located TAPs (GPS jitter can be 5-10m)
				key := fmt.Sprintf("%.4f,%.4f", td.Latitude, td.Longitude)
				if ut, ok := tapMap[key]; ok {
					ut.dist += td.DistanceM
					ut.unc += td.UncertaintyM
					ut.count++
				} else {
					tapMap[key] = &uniqueTap{
						lat: td.Latitude, lon: td.Longitude,
						dist: td.DistanceM, unc: td.UncertaintyM, count: 1,
					}
				}
			}
			// Average distances; reduce uncertainty by sqrt(N) for merged measurements
			var uniqueTaps []uniqueTap
			for _, ut := range tapMap {
				ut.dist /= float64(ut.count)
				ut.unc /= math.Sqrt(float64(ut.count)) // proper uncertainty propagation
				uniqueTaps = append(uniqueTaps, *ut)
			}

			// Decide between circle_intersection (2+ distant TAPs) and
			// range_bearing (single effective TAP position).
			useSingleTap := len(uniqueTaps) == 1
			if len(uniqueTaps) >= 2 {
				// 2+ unique positions: try circle-circle intersection.
				t1 := uniqueTaps[0]
				t2 := uniqueTaps[1]

				// Local Cartesian frame (meters) centered at t1
				mPerDegLat := 111320.0
				mPerDegLon := 111320.0 * math.Cos(t1.lat*math.Pi/180)
				dx := (t2.lon - t1.lon) * mPerDegLon
				dy := (t2.lat - t1.lat) * mPerDegLat
				D := math.Sqrt(dx*dx + dy*dy)

				d1 := t1.dist
				d2 := t2.dist

				// TAPs must be ≥100m apart for circle intersection to produce
				// meaningful geometry. Closer → fall through to range_bearing.
				if D > 100 && d1 > 0 && d2 > 0 {
					// Adjust ranges if circles don't intersect
					if d1+d2 < D {
						scale := D / (d1 + d2) * 1.01
						d1 *= scale
						d2 *= scale
					}
					if math.Abs(d1-d2) > D {
						// Contained circle: minimally adjust so they just barely intersect.
						// Preserve the larger radius (stronger signal) and only push up the smaller.
						if d1 >= d2 {
							d2 = d1 - D*0.99
							if d2 < D*0.05 {
								d2 = D * 0.05
								d1 = d2 + D*0.99
							}
						} else {
							d1 = d2 - D*0.99
							if d1 < D*0.05 {
								d1 = D * 0.05
								d2 = d1 + D*0.99
							}
						}
					}

					a := (D*D + d1*d1 - d2*d2) / (2 * D)
					hSq := d1*d1 - a*a
					if hSq < 0 {
						hSq = 0
					}
					h := math.Sqrt(hSq)

					ux, uy := dx/D, dy/D
					px, py := -uy, ux

					c1x := a*ux + h*px
					c1y := a*uy + h*py
					c2x := a*ux - h*px
					c2y := a*uy - h*py

					var estX, estY float64
					prevEst := r.state.GetDroneEstimatedPosition(resolvedID)
					if prevEst != nil && prevEst.Latitude != 0 {
						prevX := (prevEst.Longitude - t1.lon) * mPerDegLon
						prevY := (prevEst.Latitude - t1.lat) * mPerDegLat
						d1sq := (c1x-prevX)*(c1x-prevX) + (c1y-prevY)*(c1y-prevY)
						d2sq := (c2x-prevX)*(c2x-prevX) + (c2y-prevY)*(c2y-prevY)
						if d1sq <= d2sq {
							estX, estY = c1x, c1y
						} else {
							estX, estY = c2x, c2y
						}
					} else {
						estX, estY = a*ux, a*uy
					}

					estLat := t1.lat + estY/mPerDegLat
					estLon := t1.lon + estX/mPerDegLon

					rmsUnc := math.Sqrt((t1.unc*t1.unc + t2.unc*t2.unc) / 2)
					sinTheta := h / d1
					if sinTheta < 0.1 {
						sinTheta = 0.1
					}
					avgErr := rmsUnc / sinTheta
					conf := 0.35

					// Smoothing handled by Kalman filter in setFilteredPosition

					estPos := &processor.EstimatedPosition{
						Latitude:   estLat,
						Longitude:  estLon,
						ErrorM:     avgErr,
						Confidence: conf,
						TapsUsed:   len(uniqueTaps),
						Method:     "circle_intersection",
						Timestamp:  time.Now(),
					}
					r.setFilteredPosition(resolvedID, estPos)
				} else {
					// TAPs too close together — treat as single position
					useSingleTap = true
				}
			}
			if useSingleTap && len(uniqueTaps) >= 1 {
				// Single effective TAP position: place estimate at distance along
				// bearing from previous position (if any), otherwise north.
				ut := uniqueTaps[0]
				bearing := 0.0
				prevEst := r.state.GetDroneEstimatedPosition(resolvedID)
				if prevEst != nil && prevEst.Latitude != 0 {
					dLat := prevEst.Latitude - ut.lat
					dLon := prevEst.Longitude - ut.lon
					bearing = math.Atan2(dLon, dLat)
				}
				mPerDegLat := 111320.0
				mPerDegLon := 111320.0 * math.Cos(ut.lat*math.Pi/180)
				estLat := ut.lat + (ut.dist*math.Cos(bearing))/mPerDegLat
				estLon := ut.lon + (ut.dist*math.Sin(bearing))/mPerDegLon

				// Smoothing handled by Kalman filter in setFilteredPosition

				estPos := &processor.EstimatedPosition{
					Latitude:   estLat,
					Longitude:  estLon,
					ErrorM:     ut.unc,
					Confidence: 0.2,
					TapsUsed:   1,
					Method:     "range_bearing",
					Timestamp:  time.Now(),
				}
				r.setFilteredPosition(resolvedID, estPos)
			}
		}
	} else if len(rings) == 1 {
		// Single-TAP range-bearing fallback: place estimate at distance from TAP
		// along bearing from previous position (if any), otherwise north.
		ring := rings[0]
		if ring.DistanceM > 0 {
			bearing := 0.0
			prevEst := r.state.GetDroneEstimatedPosition(resolvedID)
			if prevEst != nil && prevEst.Latitude != 0 {
				dLat := prevEst.Latitude - ring.TapLat
				dLon := prevEst.Longitude - ring.TapLon
				bearing = math.Atan2(dLon, dLat)
			}
			mPerDegLat := 111320.0
			mPerDegLon := 111320.0 * math.Cos(ring.TapLat*math.Pi/180)
			estLat := ring.TapLat + (ring.DistanceM*math.Cos(bearing))/mPerDegLat
			estLon := ring.TapLon + (ring.DistanceM*math.Sin(bearing))/mPerDegLon

			unc := (ring.MaxM - ring.MinM) / 2
			if unc < 50 {
				unc = 50
			}

			// Smoothing handled by Kalman filter in setFilteredPosition

			estPos := &processor.EstimatedPosition{
				Latitude:   estLat,
				Longitude:  estLon,
				ErrorM:     unc,
				Confidence: 0.15,
				TapsUsed:   1,
				Method:     "range_bearing",
				Timestamp:  time.Now(),
			}
			r.setFilteredPosition(resolvedID, estPos)
		}
	}

	// Re-broadcast drone state after range ring/trilateration update
	r.state.BroadcastDroneUpdate(resolvedID)
}

// setFilteredPosition applies Kalman filtering to the estimated position before storing.
// Replaces raw exponential smoothing with a proper constant-velocity Kalman filter that:
// - Tracks velocity for smooth predictions between updates
// - Weights measurements by reported uncertainty
// - Rejects outliers via innovation gating (Mahalanobis distance)
// - Handles variable timesteps correctly
func (r *Receiver) setFilteredPosition(droneID string, estPos *processor.EstimatedPosition) {
	if estPos == nil {
		return
	}

	kf := r.kalman.GetOrCreate(droneID)
	filtered := kf.Update(intel.KalmanMeasurement{
		Lat:       estPos.Latitude,
		Lon:       estPos.Longitude,
		ErrorM:    estPos.ErrorM,
		Timestamp: estPos.Timestamp,
		Source:    estPos.Method,
	})

	if filtered != nil {
		estPos.Latitude = filtered.Lat
		estPos.Longitude = filtered.Lon
		// Keep the larger of Kalman uncertainty and reported uncertainty
		if filtered.ErrorM > estPos.ErrorM {
			estPos.ErrorM = filtered.ErrorM
		}
	}

	r.state.SetDroneEstimatedPosition(droneID, estPos)
}

// handleSuspectDetection processes WiFi detections that lack RemoteID or positive identification.
// SECURITY MISSION: Track real drones immediately. Reject obvious false positives.
func (r *Receiver) handleSuspectDetection(det *pb.Detection) {
	mac := det.MacAddress
	if mac == "" {
		return
	}

	// Check if this MAC is already tracked - update via UpdateDrone (not direct mutation)
	existingDrone := r.state.GetDroneByMAC(mac)
	if existingDrone != nil {
		update := &processor.Drone{
			Identifier: existingDrone.Identifier,
			MACAddress: mac,
			RSSI:       det.Rssi,
			Channel:    det.Channel,
			TapID:      det.TapId,
			LastSeen:   time.Now(),
			Timestamp:  time.Now(),
		}
		_, _ = r.state.UpdateDrone(update)
		return
	}

	// === REJECT OBVIOUS FALSE POSITIVES ===
	// Randomized MACs (locally administered bit set) are phones/laptops, not drones.
	// Real drones use manufacturer-assigned MACs.
	// Exception: DJI OcuSync uses locally administered MACs but we can identify it by protocol.
	if isRandomizedMAC(mac) && det.Source != pb.DetectionSource_SOURCE_DJI_OCUSYNC {
		// For low-confidence detections with randomized MACs, require a drone OUI or SSID
		hasDroneOUI := intel.MatchOUI(mac) != ""
		hasDroneSSID := det.Ssid != "" && intel.MatchSSID(det.Ssid) != nil
		if !hasDroneOUI && !hasDroneSSID {
			slog.Debug("Rejected randomized MAC without drone indicator",
				"mac", mac,
				"confidence", det.Confidence,
				"ssid", det.Ssid,
			)
			return
		}
	}

	// === CHECK FALSE POSITIVES DATABASE ===
	// Reject MACs that have been flagged as false positives (IoT devices, etc.)
	if r.store != nil && r.store.IsFalsePositive(mac) {
		slog.Debug("Rejected known false positive MAC",
			"mac", mac,
			"tap_id", det.TapId,
		)
		return
	}

	// === TRACK IMMEDIATELY ===
	// Enrich with OUI lookup if available
	manufacturer := "UNKNOWN"
	if ouiDesc := intel.MatchOUI(mac); ouiDesc != "" {
		if idx := strings.Index(ouiDesc, " "); idx > 0 {
			manufacturer = ouiDesc[:idx]
		} else {
			manufacturer = ouiDesc
		}
	} else if det.Source == pb.DetectionSource_SOURCE_DJI_OCUSYNC {
		manufacturer = "DJI"
	}

	// Create drone immediately
	now := time.Now()
	identifier := fmt.Sprintf("WIFI-%s", strings.ReplaceAll(mac, ":", ""))

	drone := &processor.Drone{
		Identifier:      identifier,
		MACAddress:      mac,
		Manufacturer:    manufacturer,
		Model:           "",
		SSID:            det.Ssid,
		TapID:           det.TapId,
		RSSI:            det.Rssi,
		Channel:         det.Channel,
		FirstSeen:       now,
		LastSeen:        now,
		Timestamp:       now,
		Status:          "ACTIVE",
		Classification:  "UNVERIFIED",
		TrustScore:      50,
		Confidence:      det.Confidence,
		DetectionCount:  1,
		DetectionSource: det.Source.String(),
	}

	// Run spoof detection BEFORE adding to state (avoids double broadcast)
	trustScore, spoofFlags := r.spoof.Analyze(drone)
	drone.TrustScore = trustScore
	drone.SpoofFlags = append(drone.SpoofFlags, spoofFlags...)
	drone.Classification = classifyByTrust(trustScore, drone.SpoofFlags)

	// Single UpdateDrone call with correct classification from the start
	_, _ = r.state.UpdateDrone(drone)

	// Queue for database
	if r.store != nil && r.dbQueue != nil {
		select {
		case r.dbQueue <- dbJob{drone: drone, isNew: true}:
		default:
			r.Stats.DBQueueDrops.Add(1)
			slog.Warn("DB queue full", "identifier", drone.Identifier,
				"total_drops", r.Stats.DBQueueDrops.Load())
		}
	}

	slog.Info("TRACKING NEW TARGET",
		"mac", mac,
		"identifier", drone.Identifier,
		"manufacturer", manufacturer,
		"tap_id", det.TapId,
		"rssi", det.Rssi,
		"confidence", det.Confidence,
	)
}

// handleHeartbeat processes tap heartbeats (Protobuf)
func (r *Receiver) handleHeartbeat(msg *nats.Msg) {
	r.Stats.HeartbeatsReceived.Add(1)
	var hb pb.TapHeartbeat
	if err := proto.Unmarshal(msg.Data, &hb); err != nil {
		slog.Warn("Failed to parse heartbeat", "error", err, "subject", msg.Subject)
		return
	}

	stats := hb.Stats
	var packetsCapt, packetsFilt, detSent, uptimeSec uint64
	var kernelRecv, kernelDrop uint64
	var currChan int32
	var pktPerSec float32
	var bufSize uint64
	var bufDropped, pubRetries, natsDisc, natsReconn, totalBSSIDs uint64
	var distinctCh uint32
	var bleAdverts, bleDets uint64
	var bleScanning bool
	var bleIface string
	if stats != nil {
		packetsCapt = stats.PacketsCaptured
		packetsFilt = stats.PacketsFiltered
		detSent = stats.DetectionsSent
		currChan = stats.CurrentChannel
		pktPerSec = stats.PacketsPerSecond
		uptimeSec = stats.UptimeSeconds
		kernelRecv = uint64(stats.PcapKernelReceived)
		kernelDrop = uint64(stats.PcapKernelDropped)
		bufSize = stats.BufferSize
		bufDropped = stats.BufferDropped
		pubRetries = stats.PublishRetries
		natsDisc = stats.NatsDisconnects
		natsReconn = stats.NatsReconnects
		totalBSSIDs = stats.TotalBssids
		distinctCh = stats.DistinctChannels
		bleAdverts = stats.BleAdvertisements
		bleDets = stats.BleDetections
		bleScanning = stats.BleScanning
		bleIface = stats.BleInterface
	}
	// Fall back to heartbeat-level ble_interface if stats didn't have it
	if bleIface == "" {
		bleIface = hb.BleInterface
	}

	// Log warning if kernel drops detected
	if kernelDrop > 0 {
		slog.Warn("Kernel packet drops detected",
			"tap_id", hb.TapId,
			"dropped", kernelDrop,
			"received", kernelRecv,
			"drop_rate", float64(kernelDrop)/float64(kernelRecv+kernelDrop)*100,
		)
	}

	tap := &processor.Tap{
		ID:                 hb.TapId,
		Name:               hb.TapName,
		Latitude:           hb.Latitude,
		Longitude:          hb.Longitude,
		Altitude:           hb.Altitude,
		Version:            hb.Version,
		FramesTotal:        packetsCapt,
		PacketsCaptured:    packetsCapt,
		PacketsFiltered:    packetsFilt,
		DetectionsSent:     detSent,
		CurrentChannel:     currChan,
		PacketsPerSecond:   pktPerSec,
		PcapKernelReceived: kernelRecv,
		PcapKernelDropped:  kernelDrop,
		CPUPercent:         hb.CpuPercent,
		MemoryPercent:      hb.MemoryPercent,
		Temperature:        hb.TemperatureCelsius,
		CaptureRunning:     true, // Assume running if sending heartbeats
		TapUptime:          int64(uptimeSec),
		BufferSize:         bufSize,
		BufferDropped:      bufDropped,
		PublishRetries:     pubRetries,
		NATSDisconnects:    natsDisc,
		NATSReconnects:     natsReconn,
		TotalBSSIDs:        totalBSSIDs,
		DistinctChannels:   distinctCh,
		WiFiInterface:      hb.WifiInterface,
		BLEInterface:       bleIface,
		BLEAdvertisements:  bleAdverts,
		BLEDetections:      bleDets,
		BLEScanning:        bleScanning,
	}

	r.state.UpdateTap(tap)

	// Cache TAP position for fast detection-path lookups (avoids state shard locks)
	if tap.Latitude != 0 || tap.Longitude != 0 {
		r.tapCache.Store(tap.ID, &cachedTapPos{Latitude: tap.Latitude, Longitude: tap.Longitude})
	}

	// Persist tap stats to database (rate-limited internally to once per 60s per tap)
	if r.store != nil {
		if err := r.store.SaveTapStats(tap); err != nil {
			slog.Warn("Failed to save tap stats", "tap_id", tap.ID, "error", err)
		}
	}

	slog.Debug("Tap heartbeat received", "id", tap.ID, "name", tap.Name)
}

// classifyByTrust determines threat classification based on trust score, flags, and detection context.
// Maps trust scores into operational categories that security operators can act on.
// Requires flag combinations (not single soft flags) to avoid false SUSPECT classification.
func classifyByTrust(trust int, flags []string) string {
	hasCriticalFlags := false
	spoofFlagCount := 0
	for _, f := range flags {
		switch f {
		case "coordinate_jump", "invalid_coordinates", "impossible_vendor", "duplicate_id":
			hasCriticalFlags = true
		case "oui_ssid_mismatch", "rssi_distance_mismatch", "rssi_impossible":
			spoofFlagCount++
		}
	}

	// HOSTILE: Strong evidence of spoofing or malicious behavior
	if trust < 20 || (hasCriticalFlags && trust < 40) {
		return "HOSTILE"
	}
	// SUSPECT: Requires multiple spoof indicators or low trust + any flag
	if trust < 40 || spoofFlagCount >= 2 || (spoofFlagCount >= 1 && trust < 50) || len(flags) > 3 {
		return "SUSPECT"
	}
	// UNKNOWN: Insufficient data or minor anomalies
	if trust < 60 || len(flags) > 1 {
		return "UNKNOWN"
	}
	// NEUTRAL: Reasonable trust, minor or no flags
	if trust < 80 {
		return "NEUTRAL"
	}
	// FRIENDLY: High trust, RemoteID compliant, no flags
	return "FRIENDLY"
}

// isRandomizedMAC checks if a MAC address has the locally administered bit set.
// Randomized MACs (used by phones/laptops for privacy) have bit 1 of the first octet set.
// Real drone manufacturers use globally unique (non-randomized) MACs.
// Format: "XX:XX:XX:XX:XX:XX" where first byte's bit 1 indicates local admin.
func isRandomizedMAC(mac string) bool {
	if len(mac) < 2 {
		return false
	}
	b, err := strconv.ParseUint(mac[:2], 16, 8)
	if err != nil {
		return false
	}
	return (b & 0x02) != 0
}

// Close shuts down the receiver with graceful drain
func (r *Receiver) Close() {
	// Drain NATS subscriptions (processes in-flight messages, then unsubscribes)
	r.subsMu.RLock()
	subs := make([]*nats.Subscription, len(r.subs))
	copy(subs, r.subs)
	r.subsMu.RUnlock()
	for _, sub := range subs {
		sub.Drain()
	}
	// Wait briefly for drain to complete
	time.Sleep(500 * time.Millisecond)

	// Shutdown database worker pool gracefully
	if r.dbCancel != nil {
		r.dbCancel()
	}
	if r.dbQueue != nil {
		close(r.dbQueue)
	}
	// Wait for workers to finish with timeout
	done := make(chan struct{})
	go func() {
		r.dbWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		slog.Debug("DB workers shut down cleanly")
	case <-time.After(5 * time.Second):
		slog.Warn("DB workers shutdown timed out")
	}

	r.nc.Drain()
}

// Publish sends a message to a NATS subject
func (r *Receiver) Publish(subject string, data []byte) error {
	return r.nc.Publish(subject, data)
}

// handleCommandAck processes command acknowledgments from TAPs
func (r *Receiver) handleCommandAck(msg *nats.Msg) {
	var ack pb.TapCommandAck
	if err := proto.Unmarshal(msg.Data, &ack); err != nil {
		slog.Warn("Failed to parse command ack", "error", err)
		return
	}

	slog.Info("Command acknowledgment received",
		"tap_id", ack.TapId,
		"command_id", ack.CommandId,
		"success", ack.Success,
		"error", ack.ErrorMessage,
		"latency_ms", float64(ack.LatencyNs)/1e6,
	)

	// Notify pending command if waiting
	r.pendingCmdsMu.Lock()
	if pending, ok := r.pendingCmds[ack.CommandId]; ok {
		if pending.AckCh != nil {
			select {
			case pending.AckCh <- CommandAck{
				TapID:      ack.TapId,
				CommandID:  ack.CommandId,
				Success:    ack.Success,
				Error:      ack.ErrorMessage,
				LatencyNs:  ack.LatencyNs,
				ReceivedAt: time.Now(),
			}:
			default:
			}
		}
		delete(r.pendingCmds, ack.CommandId)
	}
	r.pendingCmdsMu.Unlock()
}

// SendPingCommand sends a ping command to a TAP
func (r *Receiver) SendPingCommand(tapID string) (string, error) {
	commandID := fmt.Sprintf("ping-%d", time.Now().UnixNano())
	now := time.Now()

	cmd := &pb.TapCommand{
		TapId:       tapID,
		TimestampNs: now.UnixNano(),
		CommandId:   commandID,
		Command: &pb.TapCommand_Ping{
			Ping: &pb.PingCommand{
				SentAtNs: now.UnixNano(),
			},
		},
	}

	return r.sendCommand(tapID, commandID, "ping", cmd)
}

// SendRestartCommand sends a restart command to a TAP
func (r *Receiver) SendRestartCommand(tapID string, graceful bool) (string, error) {
	commandID := fmt.Sprintf("restart-%d", time.Now().UnixNano())

	cmd := &pb.TapCommand{
		TapId:       tapID,
		TimestampNs: time.Now().UnixNano(),
		CommandId:   commandID,
		Command: &pb.TapCommand_Restart{
			Restart: &pb.RestartCommand{
				Graceful: graceful,
			},
		},
	}

	return r.sendCommand(tapID, commandID, "restart", cmd)
}

// SendSetChannelsCommand sends a channel configuration command to a TAP
func (r *Receiver) SendSetChannelsCommand(tapID string, channels []int32, hopIntervalMs int32) (string, error) {
	commandID := fmt.Sprintf("setchan-%d", time.Now().UnixNano())

	cmd := &pb.TapCommand{
		TapId:       tapID,
		TimestampNs: time.Now().UnixNano(),
		CommandId:   commandID,
		Command: &pb.TapCommand_SetChannels{
			SetChannels: &pb.SetChannelsCommand{
				Channels:      channels,
				HopIntervalMs: hopIntervalMs,
			},
		},
	}

	return r.sendCommand(tapID, commandID, "set_channels", cmd)
}

// SendUpdateConfigCommand sends a config update command to a TAP
func (r *Receiver) SendUpdateConfigCommand(tapID string, config map[string]string) (string, error) {
	commandID := fmt.Sprintf("config-%d", time.Now().UnixNano())

	cmd := &pb.TapCommand{
		TapId:       tapID,
		TimestampNs: time.Now().UnixNano(),
		CommandId:   commandID,
		Command: &pb.TapCommand_UpdateConfig{
			UpdateConfig: &pb.UpdateConfigCommand{
				Config: config,
			},
		},
	}

	return r.sendCommand(tapID, commandID, "update_config", cmd)
}

// BroadcastCommand sends a command to all TAPs
func (r *Receiver) BroadcastCommand(cmd *pb.TapCommand) (string, error) {
	cmd.TapId = "*"
	return r.sendCommand("*", cmd.CommandId, "broadcast", cmd)
}

// sendCommand marshals and publishes a command to NATS
func (r *Receiver) sendCommand(tapID, commandID, cmdType string, cmd *pb.TapCommand) (string, error) {
	data, err := proto.Marshal(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to marshal command: %w", err)
	}

	// Determine subject
	subject := fmt.Sprintf("skylens.commands.%s", tapID)
	if tapID == "*" {
		subject = "skylens.commands.broadcast"
	}

	// Track pending command
	r.pendingCmdsMu.Lock()
	r.pendingCmds[commandID] = &PendingCommand{
		CommandID: commandID,
		TapID:     tapID,
		Command:   cmdType,
		SentAt:    time.Now(),
	}
	r.pendingCmdsMu.Unlock()

	if err := r.nc.Publish(subject, data); err != nil {
		r.pendingCmdsMu.Lock()
		delete(r.pendingCmds, commandID)
		r.pendingCmdsMu.Unlock()
		return "", fmt.Errorf("failed to publish command: %w", err)
	}

	slog.Info("Command sent",
		"subject", subject,
		"tap_id", tapID,
		"command_id", commandID,
		"type", cmdType,
	)

	return commandID, nil
}

// WaitForAck waits for a command acknowledgment with timeout
func (r *Receiver) WaitForAck(commandID string, timeout time.Duration) (*CommandAck, error) {
	ackCh := make(chan CommandAck, 1)

	r.pendingCmdsMu.Lock()
	if pending, ok := r.pendingCmds[commandID]; ok {
		pending.AckCh = ackCh
	} else {
		r.pendingCmdsMu.Unlock()
		return nil, fmt.Errorf("command not found: %s", commandID)
	}
	r.pendingCmdsMu.Unlock()

	select {
	case ack := <-ackCh:
		return &ack, nil
	case <-time.After(timeout):
		r.pendingCmdsMu.Lock()
		delete(r.pendingCmds, commandID)
		r.pendingCmdsMu.Unlock()
		return nil, fmt.Errorf("timeout waiting for ack")
	}
}

// GetRSSIStats returns RSSI tracker statistics
func (r *Receiver) GetRSSIStats() map[string]interface{} {
	return r.rssiTracker.Stats()
}

// GetDroneRSSI returns RSSI analysis for a specific drone
func (r *Receiver) GetDroneRSSI(identifier string) *intel.RSSIAnalysis {
	return r.rssiTracker.GetDroneRSSI(identifier)
}

// CleanupRSSI removes stale RSSI tracking data to prevent unbounded memory growth
func (r *Receiver) CleanupRSSI(maxAge time.Duration, maxTracked int) int {
	return r.rssiTracker.ClearStale(maxAge, maxTracked)
}

// haversineKmSimple returns the distance in kilometers between two lat/lon points.
func haversineKmSimple(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371.0 // Earth radius km
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
