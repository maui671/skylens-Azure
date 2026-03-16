package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/K13094/skylens/internal/api"
	"github.com/K13094/skylens/internal/intel"
	"github.com/K13094/skylens/internal/notify"
	"github.com/K13094/skylens/internal/processor"
	"github.com/K13094/skylens/internal/receiver"
	"github.com/K13094/skylens/internal/storage"
	"github.com/K13094/skylens/internal/tak"
)

// Lock file in working directory (must be writable under systemd ProtectHome=read-only)
const lockFile = "/var/lib/skylens/skylens-node.lock"

// lockFileHandle holds the lock file descriptor for cleanup
var lockFileHandle *os.File

// acquireLock ensures only one instance of skylens-node runs at a time using flock.
// This is more reliable than PID files because the kernel releases the lock on process exit.
func acquireLock() (func(), error) {
	// Open/create the lock file
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file %s: %w", lockFile, err)
	}

	// Try to acquire exclusive lock (non-blocking)
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		f.Close()
		// Read existing PID if available
		if data, readErr := os.ReadFile(lockFile); readErr == nil && len(data) > 0 {
			return nil, fmt.Errorf("skylens-node already running (PID %s). Cannot start second instance", strings.TrimSpace(string(data)))
		}
		return nil, fmt.Errorf("skylens-node already running. Cannot acquire lock on %s", lockFile)
	}

	// Write our PID to the lock file (for informational purposes)
	pid := os.Getpid()
	if err := f.Truncate(0); err != nil {
		slog.Warn("Failed to truncate lock file", "error", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		slog.Warn("Failed to seek lock file", "error", err)
	}
	if _, err := f.WriteString(fmt.Sprintf("%d\n", pid)); err != nil {
		slog.Warn("Failed to write PID to lock file", "error", err)
	}
	if err := f.Sync(); err != nil {
		slog.Warn("Failed to sync lock file", "error", err)
	}

	// Keep file handle open - closing it releases the lock!
	lockFileHandle = f

	slog.Info("Acquired exclusive lock", "pid", pid, "file", lockFile, "fd", f.Fd())

	// Return cleanup function
	return func() {
		if lockFileHandle != nil {
			syscall.Flock(int(lockFileHandle.Fd()), syscall.LOCK_UN)
			lockFileHandle.Close()
			lockFileHandle = nil
		}
		slog.Debug("Released lock", "file", lockFile)
	}, nil
}

var (
	version   = "0.1.0"
	buildTime = "dev"
)

func main() {
	// Flags
	configPath := flag.String("config", "configs/config.yaml", "Path to config file")
	logFormat := flag.String("log-format", "text", "Log format: text or json")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	flag.Parse()

	// Parse log level
	var level slog.Level
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	// Setup structured logging with configurable format
	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	if *logFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)

	slog.Info("Starting Skylens Node",
		"version", version,
		"build", buildTime,
	)

	// Acquire exclusive lock - ensures only one instance runs
	releaseLock, err := acquireLock()
	if err != nil {
		slog.Error("Cannot start - another instance is running", "error", err)
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	defer releaseLock()

	// Load config
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		slog.Warn("Config file not found, using defaults", "path", *configPath)
		cfg = DefaultConfig()
	}

	// Context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize storage
	dbCfg := storage.DatabaseConfig{
		Host:     cfg.Database.Host,
		Port:     cfg.Database.Port,
		Name:     cfg.Database.Name,
		User:     cfg.Database.User,
		Password: cfg.Database.Password,
		SSLMode:  cfg.Database.SSLMode,
	}
	redisCfg := storage.RedisConfig{
		URL: cfg.Redis.URL,
	}
	store, err := storage.New(ctx, dbCfg, redisCfg)
	if err != nil {
		slog.Error("Failed to initialize storage", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	// Ensure database schema exists
	if err := store.EnsureSchema(); err != nil {
		slog.Warn("Failed to ensure database schema", "error", err)
		// Continue anyway - schema might already exist or DB is optional
	} else {
		slog.Info("Database schema verified")
	}

	// Ensure authentication schema exists
	if err := store.EnsureAuthSchema(); err != nil {
		slog.Warn("Failed to ensure auth schema", "error", err)
		// Continue anyway - auth might be disabled
	} else {
		slog.Info("Authentication schema verified")
	}

	// Warm false positives cache in Redis for fast detection filtering
	if err := store.LoadFalsePositivesToCache(); err != nil {
		slog.Warn("Failed to warm false positives cache", "error", err)
	}

	// Initialize state manager with full detection config
	stateCfg := processor.DetectionConfig{
		LostThresholdSec:         cfg.Detection.LostThresholdSec,
		EvictAfterMin:            cfg.Detection.EvictAfterMin,
		TrustDecayRate:           cfg.Detection.TrustDecayRate,
		MaxHistoryHours:          cfg.Detection.MaxHistoryHours,
		MaxDisplayedDrones:       cfg.Detection.MaxDisplayedDrones,
		SpoofCheckEnabled:        cfg.Detection.SpoofCheckEnabled,
		SingleTapMode:            cfg.Detection.SingleTapMode,
		SingleTapMinObservations: cfg.Detection.SingleTapMinObservations,
		SingleTapMinTimeSpanSec:  cfg.Detection.SingleTapMinTimeSpanSec,
		SingleTapMinMobility:     cfg.Detection.SingleTapMinMobility,
	}
	state := processor.NewStateManager(stateCfg)

	slog.Info("Detection config loaded",
		"single_tap_mode", stateCfg.SingleTapMode,
		"min_observations", stateCfg.SingleTapMinObservations,
		"min_time_span_sec", stateCfg.SingleTapMinTimeSpanSec,
		"min_mobility", stateCfg.SingleTapMinMobility,
	)

	// Apply propagation/RF environment config
	applyPropagationConfig(cfg.Propagation)

	// Restore drone state from database on startup
	// Load drones seen within the max_history_hours window
	maxAge := time.Duration(cfg.Detection.MaxHistoryHours) * time.Hour
	if drones, err := store.LoadDrones(maxAge); err != nil {
		slog.Warn("Failed to load drones from database", "error", err)
	} else if len(drones) > 0 {
		count := state.HydrateDrones(drones)
		slog.Info("Restored drone state from database",
			"loaded", count,
			"max_age_hours", cfg.Detection.MaxHistoryHours,
		)
	} else {
		slog.Debug("No drones to restore from database")
	}

	// Restore track number counter from database so new drones continue the sequence
	if maxTrack, err := store.GetMaxTrackNumber(); err != nil {
		slog.Warn("Failed to get max track number", "error", err)
	} else if maxTrack > 0 {
		state.SetNextTrackNum(maxTrack)
		slog.Info("Restored track counter", "next", maxTrack+1)
	}
	// Backfill track numbers for any existing drones that don't have one (persists to DB)
	if n := state.BackfillTrackNumbers(store); n > 0 {
		slog.Info("Backfilled track numbers", "count", n)
	}

	// Initialize spoof detector
	spoof := processor.NewSpoofDetector()

	// Initialize NATS receiver
	natsCfg := receiver.NATSConfig{
		URL: cfg.NATS.URL,
	}
	recv, err := receiver.New(ctx, natsCfg, state, spoof, store, receiver.ReceiverConfig{
		DBWorkerCount: cfg.Detection.DBWorkerCount,
		DBQueueSize:   cfg.Detection.DBQueueSize,
	})
	if err != nil {
		slog.Error("Failed to initialize NATS receiver", "error", err)
		os.Exit(1)
	}
	defer recv.Close()

	// Initialize API server (HTTP + WebSocket)
	serverCfg := api.ServerConfig{
		HTTPPort:       cfg.Server.HTTPPort,
		WebSocketPort:  cfg.Server.WebSocketPort,
		AllowedOrigins: cfg.Server.AllowedOrigins,
		AuthEnabled:    cfg.Auth.Enabled,
		JWTSecret:      cfg.Auth.JWTSecret,
		SecureCookies:  cfg.Auth.SecureCookies,
		JWTExpiry:      cfg.Auth.JWTExpiry,
		RefreshExpiry:  cfg.Auth.RefreshExpiry,
		SessionExpiry:  cfg.Auth.SessionExpiry,
	}
	server := api.NewServer(serverCfg, state, store, recv)

	// Initialize Telegram notifier (always created; config can come from config.yaml or DB)
	tgCfg := notify.TelegramConfig{
		BotToken:        cfg.Telegram.BotToken,
		ChatID:          cfg.Telegram.ChatID,
		NotifyNewDrone:  cfg.Telegram.NotifyNewDrone,
		NotifySpoofing:  cfg.Telegram.NotifySpoofing,
		NotifyDroneLost: cfg.Telegram.NotifyDroneLost,
		NotifyTapStatus: cfg.Telegram.NotifyTapStatus,
	}
	// DB settings override config.yaml (dashboard-configured values take priority)
	if dbSettings, err := store.GetSettings("tg_"); err == nil && len(dbSettings) > 0 {
		if v, ok := dbSettings["tg_bot_token"]; ok && v != "" {
			tgCfg.BotToken = v
		}
		if v, ok := dbSettings["tg_chat_id"]; ok && v != "" {
			tgCfg.ChatID = v
		}
		if v, ok := dbSettings["tg_notify_new_drone"]; ok {
			tgCfg.NotifyNewDrone = v == "true"
		}
		if v, ok := dbSettings["tg_notify_spoofing"]; ok {
			tgCfg.NotifySpoofing = v == "true"
		}
		if v, ok := dbSettings["tg_notify_drone_lost"]; ok {
			tgCfg.NotifyDroneLost = v == "true"
		}
		if v, ok := dbSettings["tg_notify_tap_status"]; ok {
			tgCfg.NotifyTapStatus = v == "true"
		}
	}
	tg := notify.NewTelegram(tgCfg)
	api.SetAlertNotifier(func(a api.Alert) {
		if a.Type == "new_drone" {
			if d, ok := state.GetDrone(a.Identifier); ok {
				r := notify.DroneReport{
					TrackNumber:       d.TrackNumber,
					Designation:       d.Designation,
					DetectionSource:   d.DetectionSource,
					SerialNumber:      d.SerialNumber,
					Lat:               d.Latitude,
					Lon:               d.Longitude,
					AltitudeM:         d.AltitudeGeodetic,
					SpeedMS:           d.Speed,
					VerticalSpeedMS:   d.VerticalSpeed,
					Heading:           d.Heading,
					RSSI:              d.RSSI,
					OperationalStatus: d.OperationalStatus,
					FirstSeen:         d.FirstSeen,
					LastSeen:          d.LastSeen,
					DetectionCount:    d.DetectionCount,
					OperatorLat:       d.OperatorLatitude,
					OperatorLon:       d.OperatorLongitude,
					ReportTime:        time.Now(),
				}
				if err := tg.SendDroneReport(r); err != nil {
					slog.Warn("Telegram drone report failed", "id", a.Identifier, "error", err)
				}
				return
			}
		}
		tg.Send(notify.Alert{
			ID:         a.ID,
			Priority:   a.Priority,
			Type:       a.Type,
			Identifier: a.Identifier,
			Message:    a.Message,
			Timestamp:  a.Timestamp,
			Latitude:   a.Latitude,
			Longitude:  a.Longitude,
			MGRS:       a.MGRS,
		})
	})
	api.SetTelegramTestFn(func() error {
		return tg.SendTest()
	})
	api.SetTelegramInstance(tg)
	if tgCfg.IsConfigured() {
		slog.Info("Telegram notifications enabled", "chat_id", tgCfg.ChatID)
	}

	// Initialize TAK Server output
	takClientCfg := tak.ClientConfig{
		Address:  cfg.TAK.Address,
		UseTLS:   cfg.TAK.UseTLS,
		CertFile: cfg.TAK.CertFile,
		KeyFile:  cfg.TAK.KeyFile,
		CAFile:   cfg.TAK.CAFile,
	}
	takPubCfg := tak.PublisherConfig{
		Enabled:         cfg.TAK.Enabled,
		RateLimitSec:    cfg.TAK.RateLimitSec,
		StaleSeconds:    cfg.TAK.StaleSeconds,
		SendControllers: cfg.TAK.SendControllers,
	}
	// DB settings override config.yaml
	if dbSettings, err := store.GetSettings("tak_"); err == nil && len(dbSettings) > 0 {
		if v, ok := dbSettings["tak_enabled"]; ok {
			takPubCfg.Enabled = v == "true"
		}
		if v, ok := dbSettings["tak_address"]; ok && v != "" {
			takClientCfg.Address = v
		}
		if v, ok := dbSettings["tak_use_tls"]; ok {
			takClientCfg.UseTLS = v == "true"
		}
		if v, ok := dbSettings["tak_cert_file"]; ok && v != "" {
			takClientCfg.CertFile = v
		}
		if v, ok := dbSettings["tak_key_file"]; ok && v != "" {
			takClientCfg.KeyFile = v
		}
		if v, ok := dbSettings["tak_ca_file"]; ok && v != "" {
			takClientCfg.CAFile = v
		}
		if v, ok := dbSettings["tak_rate_limit_sec"]; ok {
			if n, err := fmt.Sscanf(v, "%d", &takPubCfg.RateLimitSec); n == 1 && err == nil {
				// parsed ok
			}
		}
		if v, ok := dbSettings["tak_stale_seconds"]; ok {
			if n, err := fmt.Sscanf(v, "%d", &takPubCfg.StaleSeconds); n == 1 && err == nil {
				// parsed ok
			}
		}
		if v, ok := dbSettings["tak_send_controllers"]; ok {
			takPubCfg.SendControllers = v == "true"
		}
	}

	takClient := tak.NewClient(takClientCfg)
	takPub := tak.NewPublisher(takClient, state, takPubCfg)
	state.Subscribe(takPub.EventChannel())
	api.SetTAKPublisher(takPub)
	api.SetTAKClient(takClient)

	// Always start both goroutines — client sleeps when no address is configured
	// and wakes on config change from dashboard. This allows enabling TAK at runtime.
	go safeGo("tak-client", func() { takClient.Start(ctx) })
	go safeGo("tak-publisher", func() { takPub.Start(ctx) })
	if takPubCfg.Enabled && takClientCfg.Address != "" {
		slog.Info("TAK output enabled", "address", takClientCfg.Address, "tls", takClientCfg.UseTLS)
	} else {
		slog.Info("TAK output disabled (configurable at /tak)")
	}

	// Start components (all goroutines wrapped with panic recovery)
	go safeGo("nats-receiver", func() { recv.Start(ctx) })
	go safeGo("api-server", func() { server.Start(ctx) })
	go safeGo("state-cleanup", func() {
		state.StartCleanup(ctx,
			time.Duration(cfg.Detection.LostThresholdSec)*time.Second,
			time.Duration(cfg.Detection.TapOfflineThreshSec)*time.Second,
		)
	})

	// Controller-UAV correlator: links controllers to their associated UAVs
	ctrlCorrelator := processor.NewControllerCorrelator(state, 10*time.Second)
	ctrlCorrelator.Start()
	defer ctrlCorrelator.Stop()

	// Suspend/resume watchdog: detects when the VM resumes after laptop close/sleep.
	// Forces NATS reconnection immediately instead of waiting for ping timeout.
	go safeGo("suspend-watchdog", func() {
		const checkInterval = 5 * time.Second
		const jumpThreshold = 30 * time.Second // >30s gap = suspend/resume
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		lastCheck := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				elapsed := now.Sub(lastCheck)
				lastCheck = now
				if elapsed > jumpThreshold {
					slog.Warn("SYSTEM RESUME DETECTED — clock jumped",
						"elapsed", elapsed.Round(time.Second),
						"expected", checkInterval,
					)
					// Force NATS to detect dead connection immediately
					if err := recv.FlushNATS(3 * time.Second); err != nil {
						slog.Warn("NATS flush failed after resume (triggering reconnect)", "error", err)
					}
					// Refresh NATS subscriptions — TCP may survive suspend but
					// subscription routing inside NATS server can break silently.
					if err := recv.RefreshSubscriptions(); err != nil {
						slog.Warn("Subscription refresh failed after resume", "error", err)
					}
					// Health-check DB connections
					if store != nil && !store.IsHealthy() {
						slog.Warn("Database connections unhealthy after resume")
					}
				}
			}
		}
	})

	// Start spoof detector cleanup goroutine (prevents memory leak from stale tracks)
	go safeGo("spoof-cleanup", func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cleaned := spoof.Cleanup(30 * time.Minute)
				if cleaned > 0 {
					slog.Debug("Cleaned stale spoof tracks", "count", cleaned)
				}
			}
		}
	})

	// Start RSSI tracker and range ring cleanup goroutine
	go safeGo("rssi-cleanup", func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Cleanup stale RSSI tracking data (prevents unbounded memory growth)
				rssiCleaned := recv.CleanupRSSI(1*time.Hour, 1000)
				if rssiCleaned > 0 {
					slog.Debug("Cleaned stale RSSI tracks", "count", rssiCleaned)
				}
				// Cleanup stale range rings (older than 5 minutes)
				state.ClearOldRangeRings(5 * time.Minute)
			}
		}
	})

	// Start data retention cleanup goroutine (removes old detections from database)
	if store != nil {
		maxAge := time.Duration(cfg.Detection.MaxHistoryHours) * time.Hour
		go safeGo("data-retention", func() {
			// Run cleanup once per hour
			ticker := time.NewTicker(1 * time.Hour)
			defer ticker.Stop()

			// Also run once at startup after a short delay
			time.Sleep(30 * time.Second)
			if deleted, err := store.CleanupOldDetections(maxAge); err != nil {
				slog.Warn("Failed to cleanup old detections", "error", err)
			} else if deleted > 0 {
				slog.Info("Cleaned old detections", "deleted", deleted, "max_age_hours", cfg.Detection.MaxHistoryHours)
			}

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if deleted, err := store.CleanupOldDetections(maxAge); err != nil {
						slog.Warn("Failed to cleanup old detections", "error", err)
					} else if deleted > 0 {
						slog.Info("Cleaned old detections", "deleted", deleted, "max_age_hours", cfg.Detection.MaxHistoryHours)
					}

					// Also cleanup old drone entries (30 days)
					if deleted, err := store.CleanupOldDrones(30 * 24 * time.Hour); err != nil {
						slog.Warn("Failed to cleanup old drones", "error", err)
					} else if deleted > 0 {
						slog.Info("Cleaned old drone entries", "deleted", deleted)
					}
				}
			}
		})
	}

	slog.Info("Skylens Node is running",
		"http", cfg.Server.HTTPPort,
		"websocket", cfg.Server.WebSocketPort,
		"nats", cfg.NATS.URL,
	)

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("Shutting down gracefully...")
	takPub.Stop()
	takClient.Stop()
	cancel()
	server.Shutdown()

	// Graceful shutdown with timeout
	shutdownDone := make(chan struct{})
	go func() {
		// Allow time for components to finish in-flight operations
		time.Sleep(2 * time.Second)
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
		slog.Info("Skylens Node stopped gracefully")
	case <-time.After(10 * time.Second):
		slog.Warn("Shutdown timed out, forcing exit")
	}
}

// safeGo wraps a goroutine function with panic recovery to prevent process crash.
// On panic, logs the error with stack trace. The goroutine does NOT restart —
// a crash in a critical goroutine should be noticed and fixed, not silently masked.
func safeGo(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("GOROUTINE PANIC — this is a bug, please report",
				"goroutine", name,
				"panic", r,
			)
		}
	}()
	fn()
}

// applyPropagationConfig sets up the RF propagation environment configuration
func applyPropagationConfig(cfg PropagationConfig) {
	// Set global environment
	if cfg.GlobalEnvironment != "" {
		env := intel.ParseEnvironmentType(cfg.GlobalEnvironment)
		intel.SetGlobalEnvironment(env)
		slog.Info("RF propagation environment configured",
			"global_environment", env.String(),
			"path_loss_n", env.PathLossExponent(),
			"shadowing_sigma_db", env.ShadowingSigma(),
		)
	}

	// Set per-tap environment overrides
	for tapID, envStr := range cfg.TapEnvironments {
		env := intel.ParseEnvironmentType(envStr)
		intel.SetTapEnvironment(tapID, env)
		slog.Info("Per-tap environment configured",
			"tap_id", tapID,
			"environment", env.String(),
			"path_loss_n", env.PathLossExponent(),
		)
	}

	// Set per-tap RSSI calibration offsets
	for tapID, offset := range cfg.TapRSSIOffsets {
		intel.SetTapRSSIOffset(tapID, offset)
		slog.Info("Per-tap RSSI offset configured",
			"tap_id", tapID,
			"offset_db", offset,
		)
	}
}
