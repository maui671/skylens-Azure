package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/K13094/skylens/internal/processor"
)

// DatabaseConfig holds PostgreSQL settings
type DatabaseConfig struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	SSLMode  string
}

// RedisConfig holds Redis settings
type RedisConfig struct {
	URL string
}

// DBMetrics tracks database worker statistics for Prometheus
type DBMetrics struct {
	DetectionsSaved     int64     // Total detections written
	DronesSaved         int64     // Total drone upserts
	FalsePositiveHits   int64     // False positive cache hits
	FalsePositiveMisses int64     // False positive cache misses
	CleanupDeleted      int64     // Total rows deleted in cleanup
	LastCleanupTime     time.Time // Time of last cleanup run
	LastCleanupDuration float64   // Duration of last cleanup in seconds
	QueryErrors         int64     // Total query errors
}

// Store handles all database operations
type Store struct {
	pg      *pgxpool.Pool
	redis   *redis.Client
	ctx     context.Context
	metrics DBMetrics
	metricsMu sync.RWMutex

	// Rate-limit detection inserts: identifier -> last insert time
	detLastInsert   map[string]time.Time
	detLastInsertMu sync.Mutex

	// Rate-limit tap stats inserts: tapID -> last insert time
	tapStatsLast   map[string]time.Time
	tapStatsLastMu sync.Mutex
}

// New creates a new store with PostgreSQL and Redis connections
func New(ctx context.Context, dbCfg DatabaseConfig, redisCfg RedisConfig) (*Store, error) {
	// Connect to PostgreSQL
	connStr := fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		dbCfg.User, dbCfg.Password, dbCfg.Host, dbCfg.Port, dbCfg.Name, dbCfg.SSLMode,
	)

	pgConfig, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		slog.Warn("PostgreSQL config parse failed, running without database", "error", err)
	}
	var pgPool *pgxpool.Pool
	if pgConfig != nil {
		pgConfig.MaxConnLifetime = 30 * time.Minute       // Recycle connections (prevents stale TCP after suspend)
		pgConfig.MaxConnIdleTime = 5 * time.Minute        // Close idle connections quickly
		pgConfig.HealthCheckPeriod = 30 * time.Second     // Proactively detect dead connections
		pgPool, err = pgxpool.NewWithConfig(ctx, pgConfig)
		if err != nil {
			slog.Warn("PostgreSQL connection failed, running without database", "error", err)
			pgPool = nil
		} else {
			slog.Info("Connected to PostgreSQL", "host", dbCfg.Host, "database", dbCfg.Name)
		}
	}

	// Connect to Redis
	opts, err := redis.ParseURL(redisCfg.URL)
	if err != nil {
		slog.Warn("Redis URL parse failed", "error", err)
		opts = &redis.Options{Addr: "localhost:6379"}
	}

	redisClient := redis.NewClient(opts)
	if _, err := redisClient.Ping(ctx).Result(); err != nil {
		slog.Warn("Redis connection failed, running without cache", "error", err)
		redisClient = nil
	} else {
		slog.Info("Connected to Redis")
	}

	return &Store{
		pg:            pgPool,
		redis:         redisClient,
		ctx:           ctx,
		detLastInsert: make(map[string]time.Time),
		tapStatsLast:  make(map[string]time.Time),
	}, nil
}

// Close closes all connections
func (s *Store) Close() {
	if s.pg != nil {
		s.pg.Close()
	}
	if s.redis != nil {
		s.redis.Close()
	}
}

// PruneRateLimitMaps removes entries older than maxAge from the detection rate-limit maps.
// Call periodically (e.g., every 10 minutes) to prevent unbounded growth.
func (s *Store) PruneRateLimitMaps(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)

	s.detLastInsertMu.Lock()
	for k, t := range s.detLastInsert {
		if t.Before(cutoff) {
			delete(s.detLastInsert, k)
		}
	}
	s.detLastInsertMu.Unlock()

	s.tapStatsLastMu.Lock()
	for k, t := range s.tapStatsLast {
		if t.Before(cutoff) {
			delete(s.tapStatsLast, k)
		}
	}
	s.tapStatsLastMu.Unlock()
}

// IsHealthy checks if database connections are working
func (s *Store) IsHealthy() bool {
	// Check PostgreSQL
	if s.pg != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.pg.Ping(ctx); err != nil {
			slog.Debug("PostgreSQL health check failed", "error", err)
			return false
		}
	}

	// Check Redis
	if s.redis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := s.redis.Ping(ctx).Result(); err != nil {
			slog.Debug("Redis health check failed", "error", err)
			return false
		}
	}

	return true
}

// SaveDetection saves a drone detection to the database
func (s *Store) SaveDetection(d *processor.Drone, isNew bool) error {
	if s.pg == nil {
		return nil // No database configured
	}

	// Use background context for database operations to avoid stale context issues
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// canonicalID tracks the identifier to use for detections table
	// Identity resolution (serial/MAC dedup) is handled by StateManager in-memory.
	// The UPSERT below merges fields on conflict, so no extra lookups needed.
	canonicalID := d.Identifier

	if isNew {
		// Insert new drone
		_, err := s.pg.Exec(ctx, `
			INSERT INTO drones (
				identifier, mac_address, serial_number, session_id, utm_id,
				designation, manufacturer, model,
				latitude, longitude, altitude_geo, altitude_pressure, height_agl, height_reference,
				speed, vertical_speed, heading,
				rssi, channel, frequency_mhz,
				operator_lat, operator_lng, operator_altitude, operator_location_type,
				operational_status, confidence, trust_score, classification,
				tap_id, first_seen, last_seen,
				is_controller, ssid, detection_source, track_number
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $30, $31, $32, $33, $34)
			ON CONFLICT (identifier) DO UPDATE SET
				-- Position fields: always update with latest
				latitude = EXCLUDED.latitude,
				longitude = EXCLUDED.longitude,
				altitude_geo = EXCLUDED.altitude_geo,
				altitude_pressure = EXCLUDED.altitude_pressure,
				height_agl = EXCLUDED.height_agl,
				height_reference = COALESCE(NULLIF(EXCLUDED.height_reference, ''), drones.height_reference),
				-- Movement fields
				speed = EXCLUDED.speed,
				vertical_speed = EXCLUDED.vertical_speed,
				heading = EXCLUDED.heading,
				-- Signal fields
				rssi = EXCLUDED.rssi,
				channel = EXCLUDED.channel,
				frequency_mhz = COALESCE(NULLIF(EXCLUDED.frequency_mhz, 0), drones.frequency_mhz),
				-- Operator location
				operator_lat = CASE WHEN EXCLUDED.operator_lat != 0 THEN EXCLUDED.operator_lat ELSE drones.operator_lat END,
				operator_lng = CASE WHEN EXCLUDED.operator_lng != 0 THEN EXCLUDED.operator_lng ELSE drones.operator_lng END,
				operator_altitude = CASE WHEN EXCLUDED.operator_altitude != 0 THEN EXCLUDED.operator_altitude ELSE drones.operator_altitude END,
				operator_location_type = COALESCE(NULLIF(EXCLUDED.operator_location_type, ''), drones.operator_location_type),
				-- Identification: preserve existing if new is empty
				mac_address = COALESCE(NULLIF(EXCLUDED.mac_address, ''), drones.mac_address),
				serial_number = COALESCE(NULLIF(EXCLUDED.serial_number, ''), drones.serial_number),
				session_id = COALESCE(NULLIF(EXCLUDED.session_id, ''), drones.session_id),
				utm_id = COALESCE(NULLIF(EXCLUDED.utm_id, ''), drones.utm_id),
				designation = COALESCE(NULLIF(EXCLUDED.designation, ''), drones.designation),
				manufacturer = COALESCE(NULLIF(EXCLUDED.manufacturer, ''), drones.manufacturer),
				model = CASE WHEN EXCLUDED.model != '' THEN EXCLUDED.model ELSE drones.model END,
				operational_status = COALESCE(NULLIF(EXCLUDED.operational_status, ''), drones.operational_status),
				-- Trust/classification: always update
				confidence = EXCLUDED.confidence,
				trust_score = EXCLUDED.trust_score,
				classification = COALESCE(NULLIF(EXCLUDED.classification, ''), drones.classification),
				-- Source tracking
				tap_id = COALESCE(NULLIF(EXCLUDED.tap_id, ''), drones.tap_id),
				last_seen = EXCLUDED.last_seen,
				-- Controller/signal metadata
				is_controller = EXCLUDED.is_controller,
				ssid = COALESCE(NULLIF(EXCLUDED.ssid, ''), drones.ssid),
				detection_source = COALESCE(NULLIF(EXCLUDED.detection_source, ''), drones.detection_source),
				-- Track number: preserve existing
				track_number = COALESCE(EXCLUDED.track_number, drones.track_number)
		`,
			d.Identifier, d.MACAddress, d.SerialNumber, d.SessionID, d.UTMID,
			d.Designation, d.Manufacturer, d.Model,
			d.Latitude, d.Longitude, d.AltitudeGeodetic, d.AltitudePressure, d.HeightAGL, d.HeightReference,
			d.Speed, d.VerticalSpeed, d.Heading,
			d.RSSI, d.Channel, d.FrequencyMHz,
			d.OperatorLatitude, d.OperatorLongitude, d.OperatorAltitude, d.OperatorLocationType,
			d.OperationalStatus, d.Confidence, d.TrustScore, d.Classification,
			d.TapID, time.Now(),
			d.IsController, d.SSID, d.DetectionSource, d.TrackNumber,
		)
		if err != nil {
			slog.Error("Failed to insert drone", "error", err, "identifier", d.Identifier)
			return err
		}
	} else {
		// Update existing drone in drones table — must match INSERT ON CONFLICT fields
		_, err := s.pg.Exec(ctx, `
			UPDATE drones SET
				-- Position fields: always update with latest
				latitude = $2, longitude = $3, altitude_geo = $4,
				altitude_pressure = $5, height_agl = $6,
				height_reference = COALESCE(NULLIF($7, ''), height_reference),
				-- Movement fields
				speed = $8, vertical_speed = $9, heading = $10,
				-- Signal fields
				rssi = $11, channel = $12,
				frequency_mhz = COALESCE(NULLIF($13, 0), frequency_mhz),
				-- Operator location: preserve existing if new is zero
				operator_lat = CASE WHEN $14::float != 0 THEN $14 ELSE operator_lat END,
				operator_lng = CASE WHEN $15::float != 0 THEN $15 ELSE operator_lng END,
				operator_altitude = CASE WHEN $16::float != 0 THEN $16 ELSE operator_altitude END,
				operator_location_type = COALESCE(NULLIF($17, ''), operator_location_type),
				-- Identification: preserve existing if new is empty
				mac_address = COALESCE(NULLIF($18, ''), mac_address),
				serial_number = COALESCE(NULLIF($19, ''), serial_number),
				utm_id = COALESCE(NULLIF($20, ''), utm_id),
				designation = COALESCE(NULLIF($21, ''), designation),
				manufacturer = COALESCE(NULLIF($22, ''), manufacturer),
				model = CASE WHEN $23 != '' THEN $23 ELSE model END,
				operational_status = COALESCE(NULLIF($24, ''), operational_status),
				-- Trust/classification: always update
				confidence = $25, trust_score = $26,
				classification = COALESCE(NULLIF($27, ''), classification),
				-- Source tracking
				tap_id = COALESCE(NULLIF($28, ''), tap_id),
				last_seen = $29,
				-- Controller/signal metadata
				is_controller = $30,
				ssid = COALESCE(NULLIF($31, ''), ssid),
				detection_source = COALESCE(NULLIF($32, ''), detection_source)
			WHERE identifier = $1
		`,
			d.Identifier,
			d.Latitude, d.Longitude, d.AltitudeGeodetic,
			d.AltitudePressure, d.HeightAGL, d.HeightReference,
			d.Speed, d.VerticalSpeed, d.Heading,
			d.RSSI, d.Channel, d.FrequencyMHz,
			d.OperatorLatitude, d.OperatorLongitude, d.OperatorAltitude, d.OperatorLocationType,
			d.MACAddress, d.SerialNumber, d.UTMID,
			d.Designation, d.Manufacturer, d.Model, d.OperationalStatus,
			d.Confidence, d.TrustScore, d.Classification,
			d.TapID, time.Now(),
			d.IsController, d.SSID, d.DetectionSource,
		)
		if err != nil {
			// Don't fail - drone might not be in drones table yet, but still save detection
			slog.Debug("Failed to update drone in drones table", "error", err, "identifier", d.Identifier)
		}
	}

	// Rate-limit detection inserts: skip if no position AND last insert <30s ago.
	// Controllers (lat=0,lon=0) and stationary drones don't need per-second time-series rows.
	// New drones always get their first row recorded.
	{
		noPos := d.Latitude == 0 && d.Longitude == 0
		s.detLastInsertMu.Lock()
		last, seen := s.detLastInsert[canonicalID]
		now := time.Now()
		if seen && noPos && now.Sub(last) < 30*time.Second {
			s.detLastInsertMu.Unlock()
			goto afterDetection
		}
		s.detLastInsert[canonicalID] = now
		s.detLastInsertMu.Unlock()
	}

	// Also insert into detections time-series table (including raw payloads for forensics)
	// Use canonicalID to ensure all detections for same physical drone have same identifier
	{
		_, err := s.pg.Exec(ctx, `
			INSERT INTO detections (
				time, tap_id, identifier, mac_address,
				latitude, longitude, altitude_geo,
				speed, vertical_speed, heading,
				rssi, channel,
				source, designation, trust_score, spoof_flags,
				raw_frame, remoteid_payload,
				operator_lat, operator_lng
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		`,
			time.Now(), d.TapID, canonicalID, d.MACAddress,
			d.Latitude, d.Longitude, d.AltitudeGeodetic,
			d.Speed, d.VerticalSpeed, d.Heading,
			d.RSSI, d.Channel,
			d.DetectionSource, d.Designation, d.TrustScore, d.SpoofFlags,
			d.RawFrame, d.RemoteIDPayload,
			d.OperatorLatitude, d.OperatorLongitude,
		)
		if err != nil {
			slog.Warn("Failed to insert detection", "error", err, "identifier", d.Identifier)
			atomic.AddInt64(&s.metrics.QueryErrors, 1)
		} else {
			atomic.AddInt64(&s.metrics.DetectionsSaved, 1)
			if isNew {
				atomic.AddInt64(&s.metrics.DronesSaved, 1)
			}
		}
	}

afterDetection:
	// Update Redis cache
	if s.redis != nil {
		s.cacheDrone(d)
	}

	return nil
}

// DetectionJob holds a drone and its new/update status for batch processing
type DetectionJob struct {
	Drone *processor.Drone
	IsNew bool
}

// SaveDetectionBatch saves multiple detections in a single pgx.Batch round-trip.
// Falls back to individual SaveDetection calls if the batch fails.
func (s *Store) SaveDetectionBatch(jobs []DetectionJob) error {
	if s.pg == nil || len(jobs) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	batch := &pgx.Batch{}

	for _, j := range jobs {
		d := j.Drone
		now := time.Now()

		// Drone UPSERT (same SQL as SaveDetection for isNew, UPDATE for existing)
		if j.IsNew {
			batch.Queue(`
				INSERT INTO drones (
					identifier, mac_address, serial_number, session_id, utm_id,
					designation, manufacturer, model,
					latitude, longitude, altitude_geo, altitude_pressure, height_agl, height_reference,
					speed, vertical_speed, heading,
					rssi, channel, frequency_mhz,
					operator_lat, operator_lng, operator_altitude, operator_location_type,
					operational_status, confidence, trust_score, classification,
					tap_id, first_seen, last_seen,
					is_controller, ssid, detection_source, track_number
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $30, $31, $32, $33, $34)
				ON CONFLICT (identifier) DO UPDATE SET
					latitude = EXCLUDED.latitude, longitude = EXCLUDED.longitude,
					altitude_geo = EXCLUDED.altitude_geo, altitude_pressure = EXCLUDED.altitude_pressure,
					height_agl = EXCLUDED.height_agl,
					height_reference = COALESCE(NULLIF(EXCLUDED.height_reference, ''), drones.height_reference),
					speed = EXCLUDED.speed, vertical_speed = EXCLUDED.vertical_speed, heading = EXCLUDED.heading,
					rssi = EXCLUDED.rssi, channel = EXCLUDED.channel,
					frequency_mhz = COALESCE(NULLIF(EXCLUDED.frequency_mhz, 0), drones.frequency_mhz),
					operator_lat = CASE WHEN EXCLUDED.operator_lat != 0 THEN EXCLUDED.operator_lat ELSE drones.operator_lat END,
					operator_lng = CASE WHEN EXCLUDED.operator_lng != 0 THEN EXCLUDED.operator_lng ELSE drones.operator_lng END,
					operator_altitude = CASE WHEN EXCLUDED.operator_altitude != 0 THEN EXCLUDED.operator_altitude ELSE drones.operator_altitude END,
					operator_location_type = COALESCE(NULLIF(EXCLUDED.operator_location_type, ''), drones.operator_location_type),
					mac_address = COALESCE(NULLIF(EXCLUDED.mac_address, ''), drones.mac_address),
					serial_number = COALESCE(NULLIF(EXCLUDED.serial_number, ''), drones.serial_number),
					session_id = COALESCE(NULLIF(EXCLUDED.session_id, ''), drones.session_id),
					utm_id = COALESCE(NULLIF(EXCLUDED.utm_id, ''), drones.utm_id),
					designation = COALESCE(NULLIF(EXCLUDED.designation, ''), drones.designation),
					manufacturer = COALESCE(NULLIF(EXCLUDED.manufacturer, ''), drones.manufacturer),
					model = CASE WHEN EXCLUDED.model != '' THEN EXCLUDED.model ELSE drones.model END,
					operational_status = COALESCE(NULLIF(EXCLUDED.operational_status, ''), drones.operational_status),
					confidence = EXCLUDED.confidence, trust_score = EXCLUDED.trust_score,
					classification = COALESCE(NULLIF(EXCLUDED.classification, ''), drones.classification),
					tap_id = COALESCE(NULLIF(EXCLUDED.tap_id, ''), drones.tap_id),
					last_seen = EXCLUDED.last_seen,
					is_controller = EXCLUDED.is_controller,
					ssid = COALESCE(NULLIF(EXCLUDED.ssid, ''), drones.ssid),
					detection_source = COALESCE(NULLIF(EXCLUDED.detection_source, ''), drones.detection_source),
					track_number = COALESCE(EXCLUDED.track_number, drones.track_number)
			`,
				d.Identifier, d.MACAddress, d.SerialNumber, d.SessionID, d.UTMID,
				d.Designation, d.Manufacturer, d.Model,
				d.Latitude, d.Longitude, d.AltitudeGeodetic, d.AltitudePressure, d.HeightAGL, d.HeightReference,
				d.Speed, d.VerticalSpeed, d.Heading,
				d.RSSI, d.Channel, d.FrequencyMHz,
				d.OperatorLatitude, d.OperatorLongitude, d.OperatorAltitude, d.OperatorLocationType,
				d.OperationalStatus, d.Confidence, d.TrustScore, d.Classification,
				d.TapID, now,
				d.IsController, d.SSID, d.DetectionSource, d.TrackNumber,
			)
		} else {
			batch.Queue(`
				UPDATE drones SET
					latitude = $2, longitude = $3, altitude_geo = $4,
					altitude_pressure = $5, height_agl = $6,
					height_reference = COALESCE(NULLIF($7, ''), height_reference),
					speed = $8, vertical_speed = $9, heading = $10,
					rssi = $11, channel = $12,
					frequency_mhz = COALESCE(NULLIF($13, 0), frequency_mhz),
					operator_lat = CASE WHEN $14::float != 0 THEN $14 ELSE operator_lat END,
					operator_lng = CASE WHEN $15::float != 0 THEN $15 ELSE operator_lng END,
					operator_altitude = CASE WHEN $16::float != 0 THEN $16 ELSE operator_altitude END,
					operator_location_type = COALESCE(NULLIF($17, ''), operator_location_type),
					mac_address = COALESCE(NULLIF($18, ''), mac_address),
					serial_number = COALESCE(NULLIF($19, ''), serial_number),
					utm_id = COALESCE(NULLIF($20, ''), utm_id),
					designation = COALESCE(NULLIF($21, ''), designation),
					manufacturer = COALESCE(NULLIF($22, ''), manufacturer),
					model = CASE WHEN $23 != '' THEN $23 ELSE model END,
					operational_status = COALESCE(NULLIF($24, ''), operational_status),
					confidence = $25, trust_score = $26,
					classification = COALESCE(NULLIF($27, ''), classification),
					tap_id = COALESCE(NULLIF($28, ''), tap_id),
					last_seen = $29,
					is_controller = $30,
					ssid = COALESCE(NULLIF($31, ''), ssid),
					detection_source = COALESCE(NULLIF($32, ''), detection_source)
				WHERE identifier = $1
			`,
				d.Identifier,
				d.Latitude, d.Longitude, d.AltitudeGeodetic,
				d.AltitudePressure, d.HeightAGL, d.HeightReference,
				d.Speed, d.VerticalSpeed, d.Heading,
				d.RSSI, d.Channel, d.FrequencyMHz,
				d.OperatorLatitude, d.OperatorLongitude, d.OperatorAltitude, d.OperatorLocationType,
				d.MACAddress, d.SerialNumber, d.UTMID,
				d.Designation, d.Manufacturer, d.Model, d.OperationalStatus,
				d.Confidence, d.TrustScore, d.Classification,
				d.TapID, now,
				d.IsController, d.SSID, d.DetectionSource,
			)
		}

		// Detection time-series insert (with rate limiting)
		canonicalID := d.Identifier
		noPos := d.Latitude == 0 && d.Longitude == 0
		s.detLastInsertMu.Lock()
		last, seen := s.detLastInsert[canonicalID]
		skipDetection := seen && noPos && now.Sub(last) < 30*time.Second
		if !skipDetection {
			s.detLastInsert[canonicalID] = now
		}
		s.detLastInsertMu.Unlock()

		if !skipDetection {
			batch.Queue(`
				INSERT INTO detections (
					time, tap_id, identifier, mac_address,
					latitude, longitude, altitude_geo,
					speed, vertical_speed, heading,
					rssi, channel,
					source, designation, trust_score, spoof_flags,
					raw_frame, remoteid_payload,
					operator_lat, operator_lng
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
			`,
				now, d.TapID, canonicalID, d.MACAddress,
				d.Latitude, d.Longitude, d.AltitudeGeodetic,
				d.Speed, d.VerticalSpeed, d.Heading,
				d.RSSI, d.Channel,
				d.DetectionSource, d.Designation, d.TrustScore, d.SpoofFlags,
				d.RawFrame, d.RemoteIDPayload,
				d.OperatorLatitude, d.OperatorLongitude,
			)
		}
	}

	// Send entire batch in one round-trip
	br := s.pg.SendBatch(ctx, batch)
	defer br.Close()

	// Read all results to check for errors
	var batchErr error
	for i := 0; i < batch.Len(); i++ {
		if _, err := br.Exec(); err != nil {
			if batchErr == nil {
				batchErr = err
			}
			atomic.AddInt64(&s.metrics.QueryErrors, 1)
		}
	}

	if batchErr != nil {
		slog.Warn("Batch save had errors, falling back to individual saves", "error", batchErr, "batch_size", len(jobs))
		br.Close() // Close batch results before fallback
		// Fallback: save individually
		for _, j := range jobs {
			if err := s.SaveDetection(j.Drone, j.IsNew); err != nil {
				slog.Error("Individual fallback save failed", "error", err, "identifier", j.Drone.Identifier)
			}
		}
		return batchErr
	}

	// Update metrics and Redis cache
	for _, j := range jobs {
		atomic.AddInt64(&s.metrics.DetectionsSaved, 1)
		if j.IsNew {
			atomic.AddInt64(&s.metrics.DronesSaved, 1)
		}
		if s.redis != nil {
			s.cacheDrone(j.Drone)
		}
	}

	return nil
}

// cacheDrone stores drone state in Redis for fast dashboard access
func (s *Store) cacheDrone(d *processor.Drone) {
	if s.redis == nil {
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, time.Second)
	defer cancel()

	key := fmt.Sprintf("drone:%s", d.Identifier)
	s.redis.HSet(ctx, key, map[string]interface{}{
		"identifier":  d.Identifier,
		"designation": d.Designation,
		"latitude":    d.Latitude,
		"longitude":   d.Longitude,
		"altitude":    d.AltitudeGeodetic,
		"speed":       d.Speed,
		"heading":     d.Heading,
		"rssi":        d.RSSI,
		"trust_score": d.TrustScore,
		"status":      d.Status,
		"last_seen":   d.LastSeen.Unix(),
	})
	s.redis.Expire(ctx, key, 5*time.Minute)
}

// EnsureSchema creates tables if they don't exist
func (s *Store) EnsureSchema() error {
	if s.pg == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()

	schema := `
	CREATE TABLE IF NOT EXISTS drones (
		identifier TEXT PRIMARY KEY,
		mac_address TEXT,
		serial_number TEXT,
		session_id TEXT,
		utm_id TEXT,
		designation TEXT,
		manufacturer TEXT,
		latitude DOUBLE PRECISION,
		longitude DOUBLE PRECISION,
		altitude_geo REAL,
		altitude_pressure REAL,
		height_agl REAL,
		height_reference TEXT,
		speed REAL,
		vertical_speed REAL,
		heading REAL,
		rssi SMALLINT,
		channel SMALLINT,
		frequency_mhz INTEGER,
		country_code TEXT,
		ht_capabilities INTEGER,
		operator_lat DOUBLE PRECISION,
		operator_lng DOUBLE PRECISION,
		operator_altitude REAL,
		operator_location_type TEXT,
		operational_status TEXT,
		confidence REAL,
		trust_score SMALLINT DEFAULT 100,
		classification TEXT DEFAULT 'UNKNOWN',
		tag TEXT,
		hidden BOOLEAN DEFAULT FALSE,
		tap_id TEXT,
		first_seen TIMESTAMPTZ,
		last_seen TIMESTAMPTZ
	);

	CREATE TABLE IF NOT EXISTS detections (
		time TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		tap_id TEXT,
		identifier TEXT NOT NULL,
		mac_address TEXT,
		latitude DOUBLE PRECISION,
		longitude DOUBLE PRECISION,
		altitude_geo REAL,
		altitude_pressure REAL,
		height_agl REAL,
		speed REAL,
		vertical_speed REAL,
		heading REAL,
		rssi SMALLINT,
		channel SMALLINT,
		frequency_mhz INTEGER,
		source TEXT,
		designation TEXT,
		confidence REAL,
		trust_score SMALLINT,
		spoof_flags TEXT[],
		raw_frame BYTEA,
		remoteid_payload BYTEA
	);

	-- Add operator location columns to detections (idempotent migration)
	ALTER TABLE detections ADD COLUMN IF NOT EXISTS operator_lat DOUBLE PRECISION;
	ALTER TABLE detections ADD COLUMN IF NOT EXISTS operator_lng DOUBLE PRECISION;

	-- Track number for sequential drone identification (TRK-001, TRK-002, ...)
	ALTER TABLE drones ADD COLUMN IF NOT EXISTS track_number INTEGER;

	-- Performance indexes for detections (4.3M rows/day at 50 det/sec)
	CREATE INDEX IF NOT EXISTS idx_detections_time ON detections (time DESC);
	CREATE INDEX IF NOT EXISTS idx_detections_identifier ON detections (identifier, time DESC);
	CREATE INDEX IF NOT EXISTS idx_detections_tap_id ON detections (tap_id, time DESC);

	-- Drone lookup indexes
	CREATE INDEX IF NOT EXISTS idx_drones_serial ON drones (serial_number) WHERE serial_number IS NOT NULL AND serial_number != '';
	CREATE INDEX IF NOT EXISTS idx_drones_mac ON drones (mac_address) WHERE mac_address IS NOT NULL AND mac_address != '';
	CREATE INDEX IF NOT EXISTS idx_drones_last_seen ON drones (last_seen DESC);

	CREATE TABLE IF NOT EXISTS tap_stats (
		time TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		tap_id TEXT NOT NULL,
		packets_captured BIGINT,
		packets_filtered BIGINT,
		detections_sent BIGINT,
		bytes_captured BIGINT,
		current_channel SMALLINT,
		packets_per_second REAL,
		uptime_seconds BIGINT,
		pcap_kernel_received BIGINT,
		pcap_kernel_dropped BIGINT,
		cpu_percent REAL,
		memory_percent REAL,
		temperature REAL
	);

	-- RSSI Calibration table for per-model RF parameters
	CREATE TABLE IF NOT EXISTS rssi_calibration (
		id SERIAL PRIMARY KEY,
		model TEXT NOT NULL,
		manufacturer TEXT,
		rssi0 DOUBLE PRECISION NOT NULL,
		path_loss_n DOUBLE PRECISION NOT NULL DEFAULT 1.8,
		shadowing_sigma DOUBLE PRECISION DEFAULT 6.0,
		tx_power_offset DOUBLE PRECISION DEFAULT 0.0,
		environment TEXT DEFAULT 'open_field',
		sample_count INTEGER DEFAULT 0,
		min_distance_m DOUBLE PRECISION,
		max_distance_m DOUBLE PRECISION,
		rmse DOUBLE PRECISION,
		notes TEXT,
		is_active BOOLEAN DEFAULT TRUE,
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW()
	);

	-- RSSI calibration data points (ground truth measurements)
	CREATE TABLE IF NOT EXISTS rssi_calibration_data (
		id SERIAL PRIMARY KEY,
		calibration_id INTEGER REFERENCES rssi_calibration(id),
		tap_id TEXT NOT NULL,
		drone_id TEXT,
		rssi DOUBLE PRECISION NOT NULL,
		ground_truth_m DOUBLE PRECISION NOT NULL,
		tap_lat DOUBLE PRECISION,
		tap_lon DOUBLE PRECISION,
		drone_lat DOUBLE PRECISION,
		drone_lon DOUBLE PRECISION,
		drone_alt DOUBLE PRECISION,
		timestamp TIMESTAMPTZ DEFAULT NOW()
	);

	-- Learned signatures table for signature learning/confirmation workflow
	CREATE TABLE IF NOT EXISTS learned_signatures (
		id SERIAL PRIMARY KEY,
		mac_prefix TEXT,
		ssid_pattern TEXT,
		channel_band TEXT,
		beacon_interval_tu INTEGER,
		manufacturer TEXT,
		model TEXT,
		confirmation_method TEXT,
		sample_count INTEGER DEFAULT 1,
		first_seen TIMESTAMPTZ DEFAULT NOW(),
		last_seen TIMESTAMPTZ DEFAULT NOW(),
		confidence REAL DEFAULT 0.7,
		is_active BOOLEAN DEFAULT TRUE
	);

	-- Server settings (key-value store for dashboard-configurable settings)
	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TIMESTAMPTZ DEFAULT NOW()
	);

	-- Alerts history
	CREATE TABLE IF NOT EXISTS alerts (
		id TEXT PRIMARY KEY,
		priority TEXT NOT NULL,
		alert_type TEXT NOT NULL,
		identifier TEXT,
		message TEXT,
		acked BOOLEAN DEFAULT FALSE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		expires_at TIMESTAMPTZ
	);

	-- False positives tracking for tuning detection
	CREATE TABLE IF NOT EXISTS false_positives (
		id SERIAL PRIMARY KEY,
		mac_address TEXT NOT NULL,
		ssid TEXT,
		flags TEXT[],
		reason TEXT,
		reported_at TIMESTAMPTZ DEFAULT NOW(),
		reporter TEXT
	);

	CREATE INDEX IF NOT EXISTS idx_drones_last_seen ON drones (last_seen DESC);
	CREATE INDEX IF NOT EXISTS idx_drones_tap_id ON drones (tap_id);
	CREATE INDEX IF NOT EXISTS idx_drones_classification ON drones (classification);
	CREATE INDEX IF NOT EXISTS idx_drones_hidden ON drones (hidden) WHERE hidden = false;
	CREATE INDEX IF NOT EXISTS idx_detections_time ON detections (time DESC);
	CREATE INDEX IF NOT EXISTS idx_detections_identifier ON detections (identifier, time DESC);
	CREATE INDEX IF NOT EXISTS idx_detections_tap_id ON detections (tap_id, time DESC);
	CREATE INDEX IF NOT EXISTS idx_tap_stats_time ON tap_stats (time DESC);
	CREATE INDEX IF NOT EXISTS idx_tap_stats_tap_id ON tap_stats (tap_id, time DESC);
	CREATE INDEX IF NOT EXISTS idx_rssi_calibration_model ON rssi_calibration (model, environment);
	CREATE INDEX IF NOT EXISTS idx_rssi_calibration_active ON rssi_calibration (is_active, model);
	CREATE INDEX IF NOT EXISTS idx_rssi_calibration_data_cal ON rssi_calibration_data (calibration_id);
	CREATE INDEX IF NOT EXISTS idx_signatures_mac ON learned_signatures (mac_prefix);
	CREATE INDEX IF NOT EXISTS idx_signatures_ssid ON learned_signatures (ssid_pattern);
	CREATE INDEX IF NOT EXISTS idx_signatures_active ON learned_signatures (is_active);
	CREATE INDEX IF NOT EXISTS idx_false_positives_mac ON false_positives (mac_address);
	CREATE INDEX IF NOT EXISTS idx_alerts_created ON alerts (created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_alerts_type ON alerts (alert_type);
	CREATE INDEX IF NOT EXISTS idx_alerts_expires ON alerts (expires_at) WHERE expires_at IS NOT NULL;
	`

	_, err := s.pg.Exec(ctx, schema)
	return err
}

// UpdateDroneClassification persists a classification change to the database
func (s *Store) UpdateDroneClassification(identifier, classification string) error {
	if s.pg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.pg.Exec(ctx,
		`UPDATE drones SET classification = $2 WHERE identifier = $1`,
		identifier, classification,
	)
	return err
}

// UpdateDroneTag persists a tag change to the database
func (s *Store) UpdateDroneTag(identifier, tag string) error {
	if s.pg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.pg.Exec(ctx,
		`UPDATE drones SET tag = $2 WHERE identifier = $1`,
		identifier, tag,
	)
	return err
}

// SaveTapStats saves tap statistics to the database (rate-limited to once per 60s per tap)
func (s *Store) SaveTapStats(t *processor.Tap) error {
	if s.pg == nil {
		return nil
	}

	// Rate-limit: one insert per tap per 60 seconds
	s.tapStatsLastMu.Lock()
	now := time.Now()
	if last, ok := s.tapStatsLast[t.ID]; ok && now.Sub(last) < 60*time.Second {
		s.tapStatsLastMu.Unlock()
		return nil
	}
	s.tapStatsLast[t.ID] = now
	s.tapStatsLastMu.Unlock()

	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	_, err := s.pg.Exec(ctx, `
		INSERT INTO tap_stats (
			time, tap_id, packets_captured, packets_filtered, detections_sent,
			current_channel, packets_per_second, uptime_seconds,
			pcap_kernel_received, pcap_kernel_dropped,
			cpu_percent, memory_percent, temperature
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`,
		time.Now(), t.ID, t.PacketsCaptured, t.PacketsFiltered, t.DetectionsSent,
		t.CurrentChannel, t.PacketsPerSecond, t.TapUptime,
		t.PcapKernelReceived, t.PcapKernelDropped,
		t.CPUPercent, t.MemoryPercent, t.Temperature,
	)
	return err
}

// FlightPosition represents a single position in flight history
type FlightPosition struct {
	Time       time.Time `json:"time"`
	Lat        float64   `json:"lat"`
	Lng        float64   `json:"lng"`
	Altitude   float32   `json:"altitude"`
	Speed      float32   `json:"speed"`
	Heading    float32   `json:"heading"`
	TrustScore int16     `json:"trust_score"`
	OpLat      float64   `json:"op_lat,omitempty"`
	OpLng      float64   `json:"op_lng,omitempty"`
}

// FlightStats contains statistics about a flight
type FlightStats struct {
	PositionCount   int     `json:"position_count"`
	TotalDistanceM  float64 `json:"total_distance_m"`
	MaxAltitudeM    float32 `json:"max_altitude_m"`
	MaxSpeedMS      float32 `json:"max_speed_ms"`
	DurationS       float64 `json:"duration_s"`
	FirstSeen       time.Time `json:"first_seen"`
	LastSeen        time.Time `json:"last_seen"`
}

// Sighting represents a single observation session (gap-based grouping)
type Sighting struct {
	Date       string   `json:"date"`
	StartTime  time.Time `json:"start_time"`
	EndTime    time.Time `json:"end_time"`
	DurationS  float64  `json:"duration_s"`
	Detections int      `json:"detections"`
	MaxAltM    float32  `json:"max_alt_m"`
	MaxSpeedMS float32  `json:"max_speed_ms"`
	AvgRSSI    int      `json:"avg_rssi"`
	TapIDs     []string `json:"tap_ids"`
}

// GetDroneSightings returns per-session sighting history for a drone, grouped by 5-minute gaps
func (s *Store) GetDroneSightings(identifier string) ([]Sighting, error) {
	if s.pg == nil {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	rows, err := s.pg.Query(ctx, `
		SELECT time, altitude_geo, speed, rssi, tap_id
		FROM detections
		WHERE identifier = $1
		ORDER BY time ASC`, identifier)
	if err != nil {
		return nil, fmt.Errorf("sightings query: %w", err)
	}
	defer rows.Close()

	const gapThreshold = 5 * time.Minute

	var sightings []Sighting
	var cur *Sighting
	var curEnd time.Time
	var rssiSum int64
	tapSet := map[string]bool{}

	for rows.Next() {
		var t time.Time
		var alt, spd float32
		var rssi int
		var tapID string
		if err := rows.Scan(&t, &alt, &spd, &rssi, &tapID); err != nil {
			continue
		}

		if cur == nil || t.Sub(curEnd) > gapThreshold {
			// finalize previous
			if cur != nil {
				cur.EndTime = curEnd
				cur.DurationS = curEnd.Sub(cur.StartTime).Seconds()
				cur.AvgRSSI = int(rssiSum / int64(cur.Detections))
				taps := make([]string, 0, len(tapSet))
				for k := range tapSet {
					taps = append(taps, k)
				}
				cur.TapIDs = taps
				sightings = append(sightings, *cur)
			}
			// start new session
			cur = &Sighting{
				Date:      t.Format("2006-01-02"),
				StartTime: t,
			}
			rssiSum = 0
			tapSet = map[string]bool{}
		}

		cur.Detections++
		curEnd = t
		rssiSum += int64(rssi)
		if alt > cur.MaxAltM {
			cur.MaxAltM = alt
		}
		if spd > cur.MaxSpeedMS {
			cur.MaxSpeedMS = spd
		}
		if tapID != "" {
			tapSet[tapID] = true
		}
	}

	// finalize last session
	if cur != nil {
		cur.EndTime = curEnd
		cur.DurationS = curEnd.Sub(cur.StartTime).Seconds()
		if cur.Detections > 0 {
			cur.AvgRSSI = int(rssiSum / int64(cur.Detections))
		}
		taps := make([]string, 0, len(tapSet))
		for k := range tapSet {
			taps = append(taps, k)
		}
		cur.TapIDs = taps
		sightings = append(sightings, *cur)
	}

	// Reverse to most recent first
	for i, j := 0, len(sightings)-1; i < j; i, j = i+1, j-1 {
		sightings[i], sightings[j] = sightings[j], sightings[i]
	}

	return sightings, nil
}

// GetFlightHistory retrieves the position history for a drone from the detections table
func (s *Store) GetFlightHistory(identifier string, limit int) ([]FlightPosition, FlightStats, error) {
	positions := []FlightPosition{}
	stats := FlightStats{}

	if s.pg == nil {
		return positions, stats, nil
	}

	if limit <= 0 || limit > 10000 {
		limit = 1000
	}

	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	// Query positions ordered by time (oldest first for proper trail rendering)
	rows, err := s.pg.Query(ctx, `
		SELECT time, latitude, longitude, altitude_geo, speed, heading, trust_score,
		       operator_lat, operator_lng
		FROM detections
		WHERE identifier = $1
		  AND latitude IS NOT NULL
		  AND longitude IS NOT NULL
		  AND latitude != 0
		  AND longitude != 0
		ORDER BY time ASC
		LIMIT $2
	`, identifier, limit)
	if err != nil {
		return positions, stats, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	var maxAlt float32
	var maxSpeed float32
	var firstTime, lastTime time.Time
	var prevLat, prevLng float64
	var totalDist float64

	for rows.Next() {
		var p FlightPosition
		var alt, spd, hdg *float32
		var ts *int16
		var opLat, opLng *float64

		if err := rows.Scan(&p.Time, &p.Lat, &p.Lng, &alt, &spd, &hdg, &ts, &opLat, &opLng); err != nil {
			continue
		}

		if alt != nil {
			p.Altitude = *alt
			if *alt > maxAlt {
				maxAlt = *alt
			}
		}
		if spd != nil {
			p.Speed = *spd
			if *spd > maxSpeed {
				maxSpeed = *spd
			}
		}
		if hdg != nil {
			p.Heading = *hdg
		}
		if ts != nil {
			p.TrustScore = *ts
		}
		if opLat != nil {
			p.OpLat = *opLat
		}
		if opLng != nil {
			p.OpLng = *opLng
		}

		// Track first/last times
		if firstTime.IsZero() {
			firstTime = p.Time
		}
		lastTime = p.Time

		// Calculate distance from previous point (Haversine)
		if prevLat != 0 && prevLng != 0 {
			totalDist += haversineDistance(prevLat, prevLng, p.Lat, p.Lng)
		}
		prevLat = p.Lat
		prevLng = p.Lng

		positions = append(positions, p)
	}

	stats = FlightStats{
		PositionCount:  len(positions),
		TotalDistanceM: totalDist,
		MaxAltitudeM:   maxAlt,
		MaxSpeedMS:     maxSpeed,
		FirstSeen:      firstTime,
		LastSeen:       lastTime,
	}
	if !firstTime.IsZero() && !lastTime.IsZero() {
		stats.DurationS = lastTime.Sub(firstTime).Seconds()
	}

	return positions, stats, nil
}

// haversineDistance calculates distance in meters between two lat/lng points
func haversineDistance(lat1, lng1, lat2, lng2 float64) float64 {
	const R = 6371000 // Earth radius in meters
	lat1Rad := lat1 * math.Pi / 180
	lat2Rad := lat2 * math.Pi / 180
	dLat := (lat2 - lat1) * math.Pi / 180
	dLng := (lng2 - lng1) * math.Pi / 180

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1Rad)*math.Cos(lat2Rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return R * c
}

// =============================================================================
// RSSI Calibration Storage
// =============================================================================

// RSSICalibration represents calibration parameters for a UAV model
type RSSICalibration struct {
	ID            int64     `json:"id"`
	Model         string    `json:"model"`           // UAV model name (e.g., "Mini 2", "Mavic 3")
	Manufacturer  string    `json:"manufacturer"`    // Manufacturer (e.g., "DJI", "Autel")
	RSSI0         float64   `json:"rssi0"`           // Reference RSSI at 1 meter
	PathLossN     float64   `json:"path_loss_n"`     // Path loss exponent
	ShadowingSigma float64  `json:"shadowing_sigma"` // Log-normal shadowing std dev (dB)
	TXPowerOffset float64   `json:"tx_power_offset"` // TX power offset from baseline
	Environment   string    `json:"environment"`     // Environment type (open_field, urban, etc.)
	SampleCount   int       `json:"sample_count"`    // Number of samples used for calibration
	MinDistanceM  float64   `json:"min_distance_m"`  // Minimum distance in calibration data
	MaxDistanceM  float64   `json:"max_distance_m"`  // Maximum distance in calibration data
	RMSE          float64   `json:"rmse"`            // Root mean square error
	Notes         string    `json:"notes"`           // Calibration notes
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	IsActive      bool      `json:"is_active"`       // Whether this calibration is active
}

// CalibrationDataPoint represents a single RSSI measurement with ground truth
type CalibrationDataPoint struct {
	ID            int64     `json:"id"`
	CalibrationID int64     `json:"calibration_id"`  // Foreign key to rssi_calibration
	TapID         string    `json:"tap_id"`
	DroneID       string    `json:"drone_id"`
	RSSI          float64   `json:"rssi"`
	GroundTruthM  float64   `json:"ground_truth_m"`  // Actual distance in meters
	TapLat        float64   `json:"tap_lat"`
	TapLon        float64   `json:"tap_lon"`
	DroneLat      float64   `json:"drone_lat"`
	DroneLon      float64   `json:"drone_lon"`
	DroneAlt      float64   `json:"drone_alt"`
	Timestamp     time.Time `json:"timestamp"`
}

// SaveCalibration saves or updates a calibration record
func (s *Store) SaveCalibration(cal *RSSICalibration) error {
	if s.pg == nil {
		return fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	if cal.ID == 0 {
		// Insert new calibration
		err := s.pg.QueryRow(ctx, `
			INSERT INTO rssi_calibration (
				model, manufacturer, rssi0, path_loss_n, shadowing_sigma,
				tx_power_offset, environment, sample_count, min_distance_m,
				max_distance_m, rmse, notes, is_active,
				created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, NOW(), NOW())
			RETURNING id
		`,
			cal.Model, cal.Manufacturer, cal.RSSI0, cal.PathLossN, cal.ShadowingSigma,
			cal.TXPowerOffset, cal.Environment, cal.SampleCount, cal.MinDistanceM,
			cal.MaxDistanceM, cal.RMSE, cal.Notes, cal.IsActive,
		).Scan(&cal.ID)
		return err
	}

	// Update existing calibration
	_, err := s.pg.Exec(ctx, `
		UPDATE rssi_calibration SET
			model = $2, manufacturer = $3, rssi0 = $4, path_loss_n = $5,
			shadowing_sigma = $6, tx_power_offset = $7, environment = $8,
			sample_count = $9, min_distance_m = $10, max_distance_m = $11,
			rmse = $12, notes = $13, is_active = $14, updated_at = NOW()
		WHERE id = $1
	`,
		cal.ID, cal.Model, cal.Manufacturer, cal.RSSI0, cal.PathLossN,
		cal.ShadowingSigma, cal.TXPowerOffset, cal.Environment,
		cal.SampleCount, cal.MinDistanceM, cal.MaxDistanceM,
		cal.RMSE, cal.Notes, cal.IsActive,
	)
	return err
}

// GetCalibration retrieves calibration for a specific model and environment
func (s *Store) GetCalibration(model, environment string) (*RSSICalibration, error) {
	if s.pg == nil {
		return nil, fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	cal := &RSSICalibration{}
	err := s.pg.QueryRow(ctx, `
		SELECT id, model, manufacturer, rssi0, path_loss_n, shadowing_sigma,
			   tx_power_offset, environment, sample_count, min_distance_m,
			   max_distance_m, rmse, notes, created_at, updated_at, is_active
		FROM rssi_calibration
		WHERE model = $1 AND environment = $2 AND is_active = true
		ORDER BY updated_at DESC
		LIMIT 1
	`, model, environment).Scan(
		&cal.ID, &cal.Model, &cal.Manufacturer, &cal.RSSI0, &cal.PathLossN,
		&cal.ShadowingSigma, &cal.TXPowerOffset, &cal.Environment,
		&cal.SampleCount, &cal.MinDistanceM, &cal.MaxDistanceM,
		&cal.RMSE, &cal.Notes, &cal.CreatedAt, &cal.UpdatedAt, &cal.IsActive,
	)
	if err != nil {
		return nil, err
	}
	return cal, nil
}

// GetCalibrationByModel retrieves the active calibration for a model (any environment)
func (s *Store) GetCalibrationByModel(model string) (*RSSICalibration, error) {
	if s.pg == nil {
		return nil, fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	cal := &RSSICalibration{}
	err := s.pg.QueryRow(ctx, `
		SELECT id, model, manufacturer, rssi0, path_loss_n, shadowing_sigma,
			   tx_power_offset, environment, sample_count, min_distance_m,
			   max_distance_m, rmse, notes, created_at, updated_at, is_active
		FROM rssi_calibration
		WHERE model = $1 AND is_active = true
		ORDER BY updated_at DESC
		LIMIT 1
	`, model).Scan(
		&cal.ID, &cal.Model, &cal.Manufacturer, &cal.RSSI0, &cal.PathLossN,
		&cal.ShadowingSigma, &cal.TXPowerOffset, &cal.Environment,
		&cal.SampleCount, &cal.MinDistanceM, &cal.MaxDistanceM,
		&cal.RMSE, &cal.Notes, &cal.CreatedAt, &cal.UpdatedAt, &cal.IsActive,
	)
	if err != nil {
		return nil, err
	}
	return cal, nil
}

// ListCalibrations returns all calibrations, optionally filtered by model
func (s *Store) ListCalibrations(modelFilter string) ([]RSSICalibration, error) {
	if s.pg == nil {
		return nil, fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	query := `
		SELECT id, model, manufacturer, rssi0, path_loss_n, shadowing_sigma,
			   tx_power_offset, environment, sample_count, min_distance_m,
			   max_distance_m, rmse, notes, created_at, updated_at, is_active
		FROM rssi_calibration
	`
	args := []interface{}{}

	if modelFilter != "" {
		query += " WHERE model ILIKE $1"
		args = append(args, "%"+modelFilter+"%")
	}

	query += " ORDER BY model, environment, updated_at DESC"

	rows, err := s.pg.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	calibrations := []RSSICalibration{}
	for rows.Next() {
		var cal RSSICalibration
		if err := rows.Scan(
			&cal.ID, &cal.Model, &cal.Manufacturer, &cal.RSSI0, &cal.PathLossN,
			&cal.ShadowingSigma, &cal.TXPowerOffset, &cal.Environment,
			&cal.SampleCount, &cal.MinDistanceM, &cal.MaxDistanceM,
			&cal.RMSE, &cal.Notes, &cal.CreatedAt, &cal.UpdatedAt, &cal.IsActive,
		); err != nil {
			continue
		}
		calibrations = append(calibrations, cal)
	}

	return calibrations, nil
}

// UpdateCalibration updates an existing calibration record
func (s *Store) UpdateCalibration(cal *RSSICalibration) error {
	return s.SaveCalibration(cal)
}

// DeleteCalibration soft-deletes a calibration by setting is_active = false
func (s *Store) DeleteCalibration(id int64) error {
	if s.pg == nil {
		return fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	_, err := s.pg.Exec(ctx, `
		UPDATE rssi_calibration SET is_active = false, updated_at = NOW()
		WHERE id = $1
	`, id)
	return err
}

// SaveCalibrationDataPoint saves a single calibration measurement
func (s *Store) SaveCalibrationDataPoint(dp *CalibrationDataPoint) error {
	if s.pg == nil {
		return fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	err := s.pg.QueryRow(ctx, `
		INSERT INTO rssi_calibration_data (
			calibration_id, tap_id, drone_id, rssi, ground_truth_m,
			tap_lat, tap_lon, drone_lat, drone_lon, drone_alt, timestamp
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id
	`,
		dp.CalibrationID, dp.TapID, dp.DroneID, dp.RSSI, dp.GroundTruthM,
		dp.TapLat, dp.TapLon, dp.DroneLat, dp.DroneLon, dp.DroneAlt, dp.Timestamp,
	).Scan(&dp.ID)
	return err
}

// GetCalibrationData retrieves all data points for a calibration
func (s *Store) GetCalibrationData(calibrationID int64) ([]CalibrationDataPoint, error) {
	if s.pg == nil {
		return nil, fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	rows, err := s.pg.Query(ctx, `
		SELECT id, calibration_id, tap_id, drone_id, rssi, ground_truth_m,
			   tap_lat, tap_lon, drone_lat, drone_lon, drone_alt, timestamp
		FROM rssi_calibration_data
		WHERE calibration_id = $1
		ORDER BY timestamp ASC
	`, calibrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	data := []CalibrationDataPoint{}
	for rows.Next() {
		var dp CalibrationDataPoint
		if err := rows.Scan(
			&dp.ID, &dp.CalibrationID, &dp.TapID, &dp.DroneID, &dp.RSSI,
			&dp.GroundTruthM, &dp.TapLat, &dp.TapLon, &dp.DroneLat,
			&dp.DroneLon, &dp.DroneAlt, &dp.Timestamp,
		); err != nil {
			continue
		}
		data = append(data, dp)
	}

	return data, nil
}

// GetRecentDetections returns detection history for analytics
func (s *Store) GetRecentDetections(hours int, limit int) ([]map[string]interface{}, error) {
	if s.pg == nil {
		return nil, fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	query := `
		SELECT
			time, identifier, mac_address,
			latitude, longitude, altitude_geo,
			speed, heading, rssi,
			trust_score, designation, source, tap_id
		FROM detections
		WHERE time > NOW() - INTERVAL '1 hour' * $1
		ORDER BY time DESC
		LIMIT $2
	`

	rows, err := s.pg.Query(ctx, query, hours, limit)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	detections := []map[string]interface{}{}
	for rows.Next() {
		var (
			ts         time.Time
			identifier string
			mac        *string
			lat, lng   *float64
			alt        *float32
			speed      *float32
			heading    *float32
			rssi       *int16
			trust      *int16
			desig      *string
			source     *string
			tapID      *string
		)

		if err := rows.Scan(&ts, &identifier, &mac, &lat, &lng, &alt,
			&speed, &heading, &rssi, &trust, &desig, &source, &tapID); err != nil {
			continue
		}

		det := map[string]interface{}{
			"identifier": identifier,
			"timestamp":  ts.Format(time.RFC3339),
			"hour":       ts.Hour(),
			"day":        int(ts.Weekday()),
		}

		if mac != nil {
			det["mac"] = *mac
		}
		if lat != nil {
			det["lat"] = *lat
		}
		if lng != nil {
			det["lng"] = *lng
		}
		if alt != nil {
			det["altitude"] = *alt
		}
		if speed != nil {
			det["speed"] = *speed
		}
		if heading != nil {
			det["heading"] = *heading
		}
		if rssi != nil {
			det["rssi"] = *rssi
		}
		if trust != nil {
			det["trust_score"] = *trust
		}
		if desig != nil {
			det["designation"] = *desig
		}
		if source != nil {
			det["source"] = *source
		}
		if tapID != nil {
			det["tap_id"] = *tapID
		}

		detections = append(detections, det)
	}

	return detections, nil
}

// CleanupOldDetections removes detections older than maxAge
// Returns the number of rows deleted
func (s *Store) CleanupOldDetections(maxAge time.Duration) (int64, error) {
	return s.CleanupOldDetectionsBatch(maxAge, 10000)
}

// CleanupOldDetectionsBatch removes detections older than maxAge in batches.
// Uses batch deletes to avoid long-running transactions and lock contention.
// Returns the total number of rows deleted.
func (s *Store) CleanupOldDetectionsBatch(maxAge time.Duration, batchSize int) (int64, error) {
	if s.pg == nil {
		return 0, nil
	}

	startTime := time.Now()
	cutoff := time.Now().Add(-maxAge)
	totalDeleted := int64(0)

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		// Delete in batches using CTID for efficient batching
		result, err := s.pg.Exec(ctx,
			`DELETE FROM detections WHERE ctid IN (
				SELECT ctid FROM detections WHERE time < $1 LIMIT $2
			)`,
			cutoff, batchSize,
		)
		cancel()

		if err != nil {
			atomic.AddInt64(&s.metrics.QueryErrors, 1)
			return totalDeleted, fmt.Errorf("delete old detections: %w", err)
		}

		deleted := result.RowsAffected()
		totalDeleted += deleted
		atomic.AddInt64(&s.metrics.CleanupDeleted, deleted)

		// If we deleted less than batch size, we're done
		if deleted < int64(batchSize) {
			break
		}

		// Small sleep between batches to avoid hammering the DB
		time.Sleep(10 * time.Millisecond)
	}

	// Update cleanup metrics
	s.metricsMu.Lock()
	s.metrics.LastCleanupTime = time.Now()
	s.metrics.LastCleanupDuration = time.Since(startTime).Seconds()
	s.metricsMu.Unlock()

	if totalDeleted > 0 {
		slog.Info("Cleaned up old detections",
			"deleted", totalDeleted,
			"cutoff", cutoff.Format(time.RFC3339),
			"duration_ms", time.Since(startTime).Milliseconds(),
		)
	}

	return totalDeleted, nil
}

// CleanupOldDrones removes drone entries not seen in maxAge
// Returns the number of rows deleted
func (s *Store) CleanupOldDrones(maxAge time.Duration) (int64, error) {
	return s.CleanupOldDronesBatch(maxAge, 1000)
}

// CleanupOldDronesBatch removes drone entries not seen in maxAge in batches.
// Returns the total number of rows deleted.
func (s *Store) CleanupOldDronesBatch(maxAge time.Duration, batchSize int) (int64, error) {
	if s.pg == nil {
		return 0, nil
	}

	cutoff := time.Now().Add(-maxAge)
	totalDeleted := int64(0)

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		result, err := s.pg.Exec(ctx,
			`DELETE FROM drones WHERE identifier IN (
				SELECT identifier FROM drones WHERE last_seen < $1 LIMIT $2
			)`,
			cutoff, batchSize,
		)
		cancel()

		if err != nil {
			atomic.AddInt64(&s.metrics.QueryErrors, 1)
			return totalDeleted, fmt.Errorf("delete old drones: %w", err)
		}

		deleted := result.RowsAffected()
		totalDeleted += deleted
		atomic.AddInt64(&s.metrics.CleanupDeleted, deleted)

		if deleted < int64(batchSize) {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	if totalDeleted > 0 {
		slog.Info("Cleaned up old drones",
			"deleted", totalDeleted,
			"cutoff", cutoff.Format(time.RFC3339),
		)
	}

	return totalDeleted, nil
}

// GetMetrics returns a copy of the current DB metrics for Prometheus scraping
func (s *Store) GetMetrics() DBMetrics {
	s.metricsMu.RLock()
	defer s.metricsMu.RUnlock()

	return DBMetrics{
		DetectionsSaved:     atomic.LoadInt64(&s.metrics.DetectionsSaved),
		DronesSaved:         atomic.LoadInt64(&s.metrics.DronesSaved),
		FalsePositiveHits:   atomic.LoadInt64(&s.metrics.FalsePositiveHits),
		FalsePositiveMisses: atomic.LoadInt64(&s.metrics.FalsePositiveMisses),
		CleanupDeleted:      atomic.LoadInt64(&s.metrics.CleanupDeleted),
		LastCleanupTime:     s.metrics.LastCleanupTime,
		LastCleanupDuration: s.metrics.LastCleanupDuration,
		QueryErrors:         atomic.LoadInt64(&s.metrics.QueryErrors),
	}
}

// =============================================================================
// Learned Signatures Storage
// =============================================================================

// LearnedSignature represents a confirmed UAV RF signature
type LearnedSignature struct {
	ID                 int64     `json:"id"`
	MACPrefix          string    `json:"mac_prefix"`           // First 3 octets (OUI) or more
	SSIDPattern        string    `json:"ssid_pattern"`         // Regex pattern for SSID matching
	ChannelBand        string    `json:"channel_band"`         // "2.4GHz", "5GHz", "both"
	BeaconIntervalTU   int       `json:"beacon_interval_tu"`   // Beacon interval in Time Units
	Manufacturer       string    `json:"manufacturer"`         // Confirmed manufacturer
	Model              string    `json:"model"`                // Confirmed model
	ConfirmationMethod string    `json:"confirmation_method"`  // "manual", "auto", "database"
	SampleCount        int       `json:"sample_count"`         // Number of samples contributing
	FirstSeen          time.Time `json:"first_seen"`
	LastSeen           time.Time `json:"last_seen"`
	Confidence         float32   `json:"confidence"`           // 0.0-1.0 confidence in signature
	IsActive           bool      `json:"is_active"`            // Whether signature is active
}

// UpsertSignature inserts or updates a learned signature
func (s *Store) UpsertSignature(sig *LearnedSignature) error {
	if s.pg == nil {
		return fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if sig.ID == 0 {
		// Check if signature with same MAC prefix exists
		var existingID int64
		err := s.pg.QueryRow(ctx, `
			SELECT id FROM learned_signatures
			WHERE mac_prefix = $1 AND is_active = true
			LIMIT 1
		`, sig.MACPrefix).Scan(&existingID)

		if err == nil {
			// Update existing
			sig.ID = existingID
		}
	}

	if sig.ID == 0 {
		// Insert new signature
		err := s.pg.QueryRow(ctx, `
			INSERT INTO learned_signatures (
				mac_prefix, ssid_pattern, channel_band, beacon_interval_tu,
				manufacturer, model, confirmation_method, sample_count,
				first_seen, last_seen, confidence, is_active
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), NOW(), $9, $10)
			RETURNING id
		`,
			sig.MACPrefix, sig.SSIDPattern, sig.ChannelBand, sig.BeaconIntervalTU,
			sig.Manufacturer, sig.Model, sig.ConfirmationMethod, sig.SampleCount,
			sig.Confidence, sig.IsActive,
		).Scan(&sig.ID)
		return err
	}

	// Update existing signature
	_, err := s.pg.Exec(ctx, `
		UPDATE learned_signatures SET
			ssid_pattern = COALESCE(NULLIF($2, ''), ssid_pattern),
			channel_band = COALESCE(NULLIF($3, ''), channel_band),
			beacon_interval_tu = COALESCE(NULLIF($4, 0), beacon_interval_tu),
			manufacturer = COALESCE(NULLIF($5, ''), manufacturer),
			model = COALESCE(NULLIF($6, ''), model),
			confirmation_method = $7,
			sample_count = sample_count + 1,
			last_seen = NOW(),
			confidence = GREATEST(confidence, $8),
			is_active = $9
		WHERE id = $1
	`,
		sig.ID, sig.SSIDPattern, sig.ChannelBand, sig.BeaconIntervalTU,
		sig.Manufacturer, sig.Model, sig.ConfirmationMethod,
		sig.Confidence, sig.IsActive,
	)
	return err
}

// GetSignatureByMAC retrieves a signature by MAC prefix
func (s *Store) GetSignatureByMAC(macPrefix string) (*LearnedSignature, error) {
	if s.pg == nil {
		return nil, fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sig := &LearnedSignature{}
	err := s.pg.QueryRow(ctx, `
		SELECT id, mac_prefix, ssid_pattern, channel_band, beacon_interval_tu,
			   manufacturer, model, confirmation_method, sample_count,
			   first_seen, last_seen, confidence, is_active
		FROM learned_signatures
		WHERE mac_prefix = $1 AND is_active = true
		ORDER BY confidence DESC, last_seen DESC
		LIMIT 1
	`, macPrefix).Scan(
		&sig.ID, &sig.MACPrefix, &sig.SSIDPattern, &sig.ChannelBand,
		&sig.BeaconIntervalTU, &sig.Manufacturer, &sig.Model,
		&sig.ConfirmationMethod, &sig.SampleCount, &sig.FirstSeen,
		&sig.LastSeen, &sig.Confidence, &sig.IsActive,
	)
	if err != nil {
		return nil, err
	}
	return sig, nil
}

// GetSignatureBySSID retrieves a signature matching an SSID pattern
func (s *Store) GetSignatureBySSID(ssid string) (*LearnedSignature, error) {
	if s.pg == nil {
		return nil, fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sig := &LearnedSignature{}
	err := s.pg.QueryRow(ctx, `
		SELECT id, mac_prefix, ssid_pattern, channel_band, beacon_interval_tu,
			   manufacturer, model, confirmation_method, sample_count,
			   first_seen, last_seen, confidence, is_active
		FROM learned_signatures
		WHERE is_active = true AND $1 ~ ssid_pattern
		ORDER BY confidence DESC, last_seen DESC
		LIMIT 1
	`, ssid).Scan(
		&sig.ID, &sig.MACPrefix, &sig.SSIDPattern, &sig.ChannelBand,
		&sig.BeaconIntervalTU, &sig.Manufacturer, &sig.Model,
		&sig.ConfirmationMethod, &sig.SampleCount, &sig.FirstSeen,
		&sig.LastSeen, &sig.Confidence, &sig.IsActive,
	)
	if err != nil {
		return nil, err
	}
	return sig, nil
}

// GetActiveSignatures returns all active learned signatures
func (s *Store) GetActiveSignatures() ([]*LearnedSignature, error) {
	if s.pg == nil {
		return nil, fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := s.pg.Query(ctx, `
		SELECT id, mac_prefix, ssid_pattern, channel_band, beacon_interval_tu,
			   manufacturer, model, confirmation_method, sample_count,
			   first_seen, last_seen, confidence, is_active
		FROM learned_signatures
		WHERE is_active = true
		ORDER BY confidence DESC, last_seen DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	signatures := []*LearnedSignature{}
	for rows.Next() {
		sig := &LearnedSignature{}
		if err := rows.Scan(
			&sig.ID, &sig.MACPrefix, &sig.SSIDPattern, &sig.ChannelBand,
			&sig.BeaconIntervalTU, &sig.Manufacturer, &sig.Model,
			&sig.ConfirmationMethod, &sig.SampleCount, &sig.FirstSeen,
			&sig.LastSeen, &sig.Confidence, &sig.IsActive,
		); err != nil {
			continue
		}
		signatures = append(signatures, sig)
	}

	return signatures, nil
}

// FalsePositive represents a recorded false positive detection
type FalsePositive struct {
	ID         int64     `json:"id"`
	MACAddress string    `json:"mac_address"`
	SSID       string    `json:"ssid,omitempty"`
	Flags      []string  `json:"flags,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	ReportedAt time.Time `json:"reported_at"`
	Reporter   string    `json:"reporter,omitempty"`
}

// RecordFalsePositive records a false positive detection for tuning
func (s *Store) RecordFalsePositive(mac string, flags []string, reason string) error {
	if s.pg == nil {
		return fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := s.pg.Exec(ctx, `
		INSERT INTO false_positives (mac_address, flags, reason, reported_at)
		VALUES ($1, $2, $3, NOW())
	`, mac, flags, reason)

	return err
}

// GetFalsePositiveCount returns the count of false positives for a MAC address
func (s *Store) GetFalsePositiveCount(mac string) (int, error) {
	if s.pg == nil {
		return 0, fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var count int
	err := s.pg.QueryRow(ctx, `
		SELECT COUNT(*) FROM false_positives WHERE mac_address = $1
	`, mac).Scan(&count)
	return count, err
}

// IsFalsePositive checks if a MAC address is in the false positives list.
// Checks both exact MAC match and OUI prefix match (first 8 chars like "00:21:7E").
// Uses Redis cache to avoid hitting Postgres on every detection.
func (s *Store) IsFalsePositive(mac string) bool {
	if mac == "" {
		return false
	}

	// Extract OUI prefix (first 8 chars: XX:XX:XX)
	ouiPrefix := ""
	if len(mac) >= 8 {
		ouiPrefix = mac[:8]
	}

	// Check Redis cache first (both full MAC and OUI prefix)
	if s.redis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		// Check cache for exact MAC
		cacheKey := "fp:" + mac
		if cached, err := s.redis.Get(ctx, cacheKey).Result(); err == nil {
			atomic.AddInt64(&s.metrics.FalsePositiveHits, 1)
			return cached == "1"
		}

		// Check cache for OUI prefix
		if ouiPrefix != "" {
			ouiCacheKey := "fp:" + ouiPrefix
			if cached, err := s.redis.Get(ctx, ouiCacheKey).Result(); err == nil && cached == "1" {
				atomic.AddInt64(&s.metrics.FalsePositiveHits, 1)
				return true
			}
		}
		atomic.AddInt64(&s.metrics.FalsePositiveMisses, 1)
	}

	// Fall back to Postgres if no cache hit
	if s.pg == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var count int
	err := s.pg.QueryRow(ctx, `
		SELECT COUNT(*) FROM false_positives
		WHERE mac_address = $1 OR mac_address = $2
	`, mac, ouiPrefix).Scan(&count)

	if err != nil {
		return false
	}

	isFP := count > 0

	// Cache the result in Redis (10 minute TTL)
	if s.redis != nil {
		cacheCtx, cacheCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cacheCancel()

		cacheVal := "0"
		if isFP {
			cacheVal = "1"
		}
		s.redis.Set(cacheCtx, "fp:"+mac, cacheVal, 10*time.Minute)
	}

	return isFP
}

// InvalidateFalsePositiveCache clears the false positive cache for a MAC/OUI.
// Call this when adding or removing entries from the false_positives table.
func (s *Store) InvalidateFalsePositiveCache(mac string) {
	if s.redis == nil || mac == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Delete both exact and OUI-prefix cache entries
	s.redis.Del(ctx, "fp:"+mac)
	if len(mac) >= 8 {
		s.redis.Del(ctx, "fp:"+mac[:8])
	}
}

// LoadFalsePositivesToCache preloads all false positives into Redis cache.
// Call this on startup to warm the cache.
func (s *Store) LoadFalsePositivesToCache() error {
	if s.pg == nil || s.redis == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := s.pg.Query(ctx, `SELECT DISTINCT mac_address FROM false_positives`)
	if err != nil {
		return fmt.Errorf("query false positives: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var mac string
		if err := rows.Scan(&mac); err != nil {
			continue
		}
		s.redis.Set(ctx, "fp:"+mac, "1", 10*time.Minute)
		count++
	}

	if count > 0 {
		slog.Info("Loaded false positives into Redis cache", "count", count)
	}

	return nil
}

// DeactivateSignature deactivates a learned signature (soft delete)
func (s *Store) DeactivateSignature(id int64) error {
	if s.pg == nil {
		return fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := s.pg.Exec(ctx, `
		UPDATE learned_signatures SET is_active = false WHERE id = $1
	`, id)
	return err
}

// DeactivateSignatureByMAC deactivates a signature by MAC prefix
func (s *Store) DeactivateSignatureByMAC(macPrefix string) error {
	if s.pg == nil {
		return fmt.Errorf("database not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := s.pg.Exec(ctx, `
		UPDATE learned_signatures SET is_active = false WHERE mac_prefix = $1
	`, macPrefix)
	return err
}

// =============================================================================
// State Restoration on Startup
// =============================================================================

// LoadDrones loads all drones from the database for state restoration on startup.
// Only loads drones seen within maxAge to avoid loading stale data.
// Returns a slice of Drone structs that can be used to hydrate the StateManager.
func (s *Store) LoadDrones(maxAge time.Duration) ([]*processor.Drone, error) {
	if s.pg == nil {
		return nil, nil // No database, return empty (not an error)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cutoff := time.Now().Add(-maxAge)

	rows, err := s.pg.Query(ctx, `
		SELECT
			identifier, mac_address, serial_number, session_id, utm_id,
			designation, manufacturer, model,
			latitude, longitude, altitude_geo, altitude_pressure, height_agl, height_reference,
			speed, vertical_speed, heading,
			rssi, channel, frequency_mhz,
			operator_lat, operator_lng, operator_altitude, operator_location_type,
			operational_status, confidence, trust_score, classification, tag, hidden,
			tap_id, first_seen, last_seen,
			is_controller, ssid, detection_source, track_number
		FROM drones
		WHERE last_seen >= $1
		ORDER BY last_seen DESC
	`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query drones: %w", err)
	}
	defer rows.Close()

	drones := []*processor.Drone{}
	for rows.Next() {
		d := &processor.Drone{}
		var (
			mac, serial, sessionID, utmID             *string
			designation, manufacturer, model           *string
			lat, lng                                   *float64
			altGeo, altPress, heightAGL                *float32
			heightRef                                  *string
			speed, vSpeed, heading                     *float32
			rssi, channel                              *int16
			freqMHz                                    *int32
			opLat, opLng                               *float64
			opAlt                                      *float32
			opLocType                                  *string
			opStatus                                   *string
			confidence                                 *float32
			trustScore                                 *int16
			classification, tag                        *string
			hidden                                     *bool
			tapID                                      *string
			firstSeen, lastSeen                        *time.Time
			isController                               *bool
			ssid, detectionSource                      *string
			trackNumber                                *int32
		)

		if err := rows.Scan(
			&d.Identifier, &mac, &serial, &sessionID, &utmID,
			&designation, &manufacturer, &model,
			&lat, &lng, &altGeo, &altPress, &heightAGL, &heightRef,
			&speed, &vSpeed, &heading,
			&rssi, &channel, &freqMHz,
			&opLat, &opLng, &opAlt, &opLocType,
			&opStatus, &confidence, &trustScore, &classification, &tag, &hidden,
			&tapID, &firstSeen, &lastSeen,
			&isController, &ssid, &detectionSource, &trackNumber,
		); err != nil {
			slog.Warn("Failed to scan drone row", "error", err)
			continue
		}

		// Populate optional fields
		if mac != nil {
			d.MACAddress = *mac
		}
		if serial != nil {
			d.SerialNumber = *serial
		}
		if sessionID != nil {
			d.SessionID = *sessionID
		}
		if utmID != nil {
			d.UTMID = *utmID
		}
		if designation != nil {
			d.Designation = *designation
		}
		if manufacturer != nil {
			d.Manufacturer = *manufacturer
		}
		if model != nil {
			d.Model = *model
		}
		if lat != nil {
			d.Latitude = *lat
		}
		if lng != nil {
			d.Longitude = *lng
		}
		if d.Latitude != 0 && d.Longitude != 0 {
			d.LastGPSTime = time.Now() // Fresh window so beacons don't immediately wipe DB GPS
		}
		if altGeo != nil {
			d.AltitudeGeodetic = *altGeo
		}
		if altPress != nil {
			d.AltitudePressure = *altPress
		}
		if heightAGL != nil {
			d.HeightAGL = *heightAGL
		}
		if heightRef != nil {
			d.HeightReference = *heightRef
		}
		if speed != nil {
			d.Speed = *speed
		}
		if vSpeed != nil {
			d.VerticalSpeed = *vSpeed
		}
		if heading != nil {
			d.Heading = *heading
			d.GroundTrack = *heading
		}
		if rssi != nil {
			d.RSSI = int32(*rssi)
		}
		if channel != nil {
			d.Channel = int32(*channel)
		}
		if freqMHz != nil {
			d.FrequencyMHz = *freqMHz
		}
		if opLat != nil {
			d.OperatorLatitude = *opLat
		}
		if opLng != nil {
			d.OperatorLongitude = *opLng
		}
		if opAlt != nil {
			d.OperatorAltitude = *opAlt
		}
		if opLocType != nil {
			d.OperatorLocationType = *opLocType
		}
		if opStatus != nil {
			// Normalize raw integer strings from legacy data (e.g. "15" → "OP_STATUS_UNKNOWN")
			switch *opStatus {
			case "OP_STATUS_UNKNOWN", "OP_STATUS_UNDECLARED", "OP_STATUS_GROUND",
				"OP_STATUS_AIRBORNE", "OP_STATUS_EMERGENCY", "OP_STATUS_REMOTE_ID_FAILURE",
				"AIRBORNE", "GROUND", "EMERGENCY", "":
				d.OperationalStatus = *opStatus
			default:
				d.OperationalStatus = "OP_STATUS_UNKNOWN"
			}
		}
		if confidence != nil {
			d.Confidence = *confidence
		}
		if trustScore != nil {
			d.TrustScore = int(*trustScore)
		}
		if classification != nil {
			d.Classification = *classification
		}
		if tag != nil {
			d.Tag = *tag
		}
		if hidden != nil {
			d.Hidden = *hidden
		}
		if tapID != nil {
			d.TapID = *tapID
		}
		if firstSeen != nil {
			d.FirstSeen = *firstSeen
		}
		if lastSeen != nil {
			d.LastSeen = *lastSeen
			d.Timestamp = *lastSeen
		}

		if isController != nil {
			d.IsController = *isController
		}
		if ssid != nil {
			d.SSID = *ssid
		}
		if detectionSource != nil {
			d.DetectionSource = *detectionSource
		}
		if trackNumber != nil {
			d.TrackNumber = int(*trackNumber)
		}

		// Determine status based on last_seen
		lostThreshold := 5 * time.Minute // Default threshold
		if lastSeen != nil && time.Since(*lastSeen) > lostThreshold {
			d.Status = "lost"
			d.ContactStatus = "lost"
		} else {
			d.Status = "active"
			d.ContactStatus = "active"
		}

		drones = append(drones, d)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate drone rows: %w", err)
	}

	return drones, nil
}

// GetMaxTrackNumber returns the highest track_number in the drones table.
// Used on startup to restore the track counter so new drones continue the sequence.
func (s *Store) GetMaxTrackNumber() (int, error) {
	if s.pg == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var max int
	err := s.pg.QueryRow(ctx, `SELECT COALESCE(MAX(track_number), 0) FROM drones`).Scan(&max)
	return max, err
}

// UpdateTrackNumber sets the track_number for a specific drone identifier.
func (s *Store) UpdateTrackNumber(identifier string, trackNumber int) error {
	if s.pg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := s.pg.Exec(ctx,
		`UPDATE drones SET track_number = $1 WHERE identifier = $2`,
		trackNumber, identifier)
	return err
}

// =============================================================================
// Alert Persistence
// =============================================================================

// AlertRow represents an alert stored in the database
type AlertRow struct {
	ID        string
	Priority  string
	AlertType string
	Identifier string
	Message   string
	Acked     bool
	CreatedAt time.Time
	ExpiresAt time.Time
}

// InsertAlert persists a new alert to the database
func (s *Store) InsertAlert(a AlertRow) error {
	if s.pg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	_, err := s.pg.Exec(ctx,
		`INSERT INTO alerts (id, priority, alert_type, identifier, message, acked, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (id) DO NOTHING`,
		a.ID, a.Priority, a.AlertType, a.Identifier, a.Message, a.Acked, a.CreatedAt, a.ExpiresAt,
	)
	return err
}

// AckAlert marks an alert as acknowledged in the database
func (s *Store) AckAlert(id string) error {
	if s.pg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	_, err := s.pg.Exec(ctx, `UPDATE alerts SET acked = TRUE WHERE id = $1`, id)
	return err
}

// AckAllAlerts marks all alerts as acknowledged
func (s *Store) AckAllAlerts() error {
	if s.pg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	_, err := s.pg.Exec(ctx, `UPDATE alerts SET acked = TRUE WHERE acked = FALSE`)
	return err
}

// ClearAlerts deletes all alerts from the database
func (s *Store) ClearAlerts() error {
	if s.pg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	_, err := s.pg.Exec(ctx, `DELETE FROM alerts`)
	return err
}

// LoadAlerts loads non-expired alerts from the database (newest first, up to limit)
func (s *Store) LoadAlerts(limit int) ([]AlertRow, error) {
	if s.pg == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	rows, err := s.pg.Query(ctx,
		`SELECT id, priority, alert_type, identifier, message, acked, created_at, expires_at
		 FROM alerts
		 WHERE expires_at > NOW()
		 ORDER BY created_at DESC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("query alerts: %w", err)
	}
	defer rows.Close()

	var result []AlertRow
	for rows.Next() {
		var a AlertRow
		if err := rows.Scan(&a.ID, &a.Priority, &a.AlertType, &a.Identifier, &a.Message, &a.Acked, &a.CreatedAt, &a.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

// CleanupExpiredAlerts removes expired alerts from the database
func (s *Store) CleanupExpiredAlerts() (int64, error) {
	if s.pg == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	tag, err := s.pg.Exec(ctx, `DELETE FROM alerts WHERE expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// =============================================================================
// Settings (key-value store)
// =============================================================================

// GetSetting retrieves a setting value by key
func (s *Store) GetSetting(key string) (string, error) {
	if s.pg == nil {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	var value string
	err := s.pg.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&value)
	if err != nil {
		return "", err
	}
	return value, nil
}

// SetSetting upserts a setting value
func (s *Store) SetSetting(key, value string) error {
	if s.pg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	_, err := s.pg.Exec(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, NOW())
		 ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW()`,
		key, value)
	return err
}

// GetUserPreferences retrieves the JSONB preferences blob for a user
func (s *Store) GetUserPreferences(userID int) (map[string]interface{}, error) {
	if s.pg == nil {
		return map[string]interface{}{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var prefs map[string]interface{}
	err := s.pg.QueryRow(ctx,
		`SELECT COALESCE(preferences, '{}'::jsonb) FROM users WHERE id = $1`,
		userID).Scan(&prefs)
	if err != nil {
		return nil, fmt.Errorf("get user preferences: %w", err)
	}
	return prefs, nil
}

// SetUserPreferences merges partial preferences into the user's existing JSONB blob
func (s *Store) SetUserPreferences(userID int, prefs map[string]interface{}) error {
	if s.pg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prefsJSON, err := json.Marshal(prefs)
	if err != nil {
		return fmt.Errorf("marshal preferences: %w", err)
	}

	_, err = s.pg.Exec(ctx,
		`UPDATE users SET preferences = COALESCE(preferences, '{}'::jsonb) || $1::jsonb WHERE id = $2`,
		prefsJSON, userID)
	if err != nil {
		return fmt.Errorf("set user preferences: %w", err)
	}
	return nil
}

// GetSettings retrieves multiple settings by key prefix
func (s *Store) GetSettings(prefix string) (map[string]string, error) {
	if s.pg == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	rows, err := s.pg.Query(ctx,
		`SELECT key, value FROM settings WHERE key LIKE $1`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		result[k] = v
	}
	return result, rows.Err()
}
