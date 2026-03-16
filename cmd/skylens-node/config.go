package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server      ServerConfig      `yaml:"server"`
	NATS        NATSConfig        `yaml:"nats"`
	Database    DatabaseConfig    `yaml:"database"`
	Redis       RedisConfig       `yaml:"redis"`
	Detection   DetectionConfig   `yaml:"detection"`
	Propagation PropagationConfig `yaml:"propagation"`
	Auth        AuthConfig        `yaml:"auth"`
	Telegram    TelegramConfig    `yaml:"telegram"`
	TAK         TAKConfig         `yaml:"tak"`
}

// TAKConfig holds TAK Server output settings
type TAKConfig struct {
	Enabled         bool   `yaml:"enabled"`
	Address         string `yaml:"address"`           // host:port
	UseTLS          bool   `yaml:"use_tls"`
	CertFile        string `yaml:"cert_file"`         // client cert PEM path
	KeyFile         string `yaml:"key_file"`           // client key PEM path
	CAFile          string `yaml:"ca_file"`             // CA cert PEM path
	RateLimitSec    int    `yaml:"rate_limit_sec"`
	StaleSeconds    int    `yaml:"stale_seconds"`
	SendControllers bool   `yaml:"send_controllers"`
}

// TelegramConfig holds Telegram bot notification settings
type TelegramConfig struct {
	Enabled   bool   `yaml:"enabled"`
	BotToken  string `yaml:"bot_token"`
	ChatID    string `yaml:"chat_id"`

	// Per-alert-type toggles (all default true)
	NotifyNewDrone  bool `yaml:"notify_new_drone"`
	NotifySpoofing  bool `yaml:"notify_spoofing"`
	NotifyDroneLost bool `yaml:"notify_drone_lost"`
	NotifyTapStatus bool `yaml:"notify_tap_status"`
}

// AuthConfig holds authentication settings
type AuthConfig struct {
	Enabled        bool          `yaml:"enabled"`
	JWTSecret      string        `yaml:"jwt_secret"`
	SecureCookies  bool          `yaml:"secure_cookies"`
	JWTExpiry      time.Duration `yaml:"jwt_expiry"`
	RefreshExpiry  time.Duration `yaml:"refresh_expiry"`
	SessionExpiry  time.Duration `yaml:"session_expiry"`
}

// PropagationConfig holds RF propagation model settings
type PropagationConfig struct {
	// GlobalEnvironment sets the default environment for all TAPs
	// Options: "open_field", "suburban", "urban", "dense_urban", "indoor"
	GlobalEnvironment string `yaml:"global_environment"`

	// TapEnvironments allows per-tap environment overrides
	// Key: tap_id, Value: environment type
	TapEnvironments map[string]string `yaml:"tap_environments"`

	// TapRSSIOffsets holds per-TAP RSSI calibration offsets in dB.
	// Corrects for adapter sensitivity differences between TAPs.
	// Positive value = TAP reads weaker than reference, offset added to normalize.
	// Calibrate by comparing co-located TAPs seeing the same signal.
	TapRSSIOffsets map[string]float64 `yaml:"tap_rssi_offsets"`

	// PathLossOverride allows manual override of path loss exponent (0 = use environment default)
	PathLossOverride float64 `yaml:"path_loss_override"`

	// ShadowingSigmaOverride allows manual override of shadowing sigma (0 = use environment default)
	ShadowingSigmaOverride float64 `yaml:"shadowing_sigma_override"`
}

type ServerConfig struct {
	HTTPPort       int      `yaml:"http_port"`
	WebSocketPort  int      `yaml:"websocket_port"`
	APIKey         string   `yaml:"api_key"`
	AllowedOrigins []string `yaml:"allowed_origins"` // WebSocket allowed origins (empty = same-origin only)
}

type NATSConfig struct {
	URL string `yaml:"url"`
}

type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Name     string `yaml:"name"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	SSLMode  string `yaml:"ssl_mode"`
}

type RedisConfig struct {
	URL string `yaml:"url"`
}

type DetectionConfig struct {
	LostThresholdSec    int     `yaml:"lost_threshold_sec"`
	EvictAfterMin       int     `yaml:"evict_after_min"`           // Minutes to keep lost drones in memory (default: 30)
	TapOfflineThreshSec int     `yaml:"tap_offline_threshold_sec"` // Seconds before TAP marked offline (default: 90)
	TrustDecayRate      float64 `yaml:"trust_decay_rate"`
	MaxHistoryHours     int     `yaml:"max_history_hours"`
	MaxDisplayedDrones  int     `yaml:"max_displayed_drones"`
	SpoofCheckEnabled   bool    `yaml:"spoof_check_enabled"`

	// DB worker pool tuning
	DBWorkerCount int `yaml:"db_worker_count"` // Parallel DB writers (default: 8)
	DBQueueSize   int `yaml:"db_queue_size"`   // DB job queue buffer (default: 5000)

	// SingleTapMode enables promotion of suspects with only one TAP.
	// When true, suspects can be auto-promoted based on:
	// - High mobility score (RSSI variance suggesting movement)
	// - Multiple observations over time (>= minObs, >= minTimeSpan)
	// Multi-TAP correlation remains a confidence booster, not a gate.
	SingleTapMode            bool    `yaml:"single_tap_mode"`
	SingleTapMinObservations int     `yaml:"single_tap_min_observations"`
	SingleTapMinTimeSpanSec  int     `yaml:"single_tap_min_time_span_sec"`
	SingleTapMinMobility     float64 `yaml:"single_tap_min_mobility"`
}

func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			HTTPPort:      8080,
			WebSocketPort: 8081,
		},
		NATS: NATSConfig{
			URL: "nats://localhost:4222",
		},
		Database: DatabaseConfig{
			Host:     "localhost",
			Port:     5432,
			Name:     "skylens",
			User:     "skylens",
			Password: "",
			SSLMode:  "disable",
		},
		Redis: RedisConfig{
			URL: "redis://localhost:6379",
		},
		Detection: DetectionConfig{
			LostThresholdSec:         300,
			EvictAfterMin:            30,    // Evict lost drones after 30 minutes
			TapOfflineThreshSec:      90,    // 90s before TAP marked offline (prevents flapping)
			TrustDecayRate:           0.1,
			MaxHistoryHours:          168,
			MaxDisplayedDrones:       500,
			SpoofCheckEnabled:        true,
			DBWorkerCount:            8,     // Parallel DB writers
			DBQueueSize:              5000,  // Buffer for burst traffic
			SingleTapMode:            true,  // Enable single-TAP promotion by default
			SingleTapMinObservations: 3,     // Minimum 3 observations (lowered for faster detection)
			SingleTapMinTimeSpanSec:  15,    // Over at least 15 seconds (lowered for faster detection)
			SingleTapMinMobility:     0.35,  // Minimum mobility score (lowered to catch slower drones)
		},
		Propagation: PropagationConfig{
			GlobalEnvironment: "suburban", // Conservative default (better than assuming open field)
			TapEnvironments:   make(map[string]string),
		},
		Auth: AuthConfig{
			Enabled:       true,  // Auth enabled by default
			JWTSecret:     "",    // Will be auto-generated if empty
			SecureCookies: false, // Set to true for HTTPS deployments
		},
		Telegram: TelegramConfig{
			Enabled:         false,
			NotifyNewDrone:  true,
			NotifySpoofing:  true,
			NotifyDroneLost: true,
			NotifyTapStatus: true,
		},
		TAK: TAKConfig{
			Enabled:         false,
			UseTLS:          true,
			RateLimitSec:    3,
			StaleSeconds:    30,
			SendControllers: false,
		},
	}
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// Override with environment variables
	if v := os.Getenv("SKYLENS_NATS_URL"); v != "" {
		cfg.NATS.URL = v
	}
	if v := os.Getenv("SKYLENS_DB_PASSWORD"); v != "" {
		cfg.Database.Password = v
	}
	if v := os.Getenv("SKYLENS_REDIS_URL"); v != "" {
		cfg.Redis.URL = v
	}
	if v := os.Getenv("SKYLENS_API_KEY"); v != "" {
		cfg.Server.APIKey = v
	}
	if v := os.Getenv("SKYLENS_JWT_SECRET"); v != "" {
		cfg.Auth.JWTSecret = v
	}
	if v := os.Getenv("SKYLENS_ALLOWED_ORIGINS"); v != "" {
		cfg.Server.AllowedOrigins = strings.Split(v, ",")
	}

	// Auto-generate JWT secret if auth is enabled but no secret is set
	if cfg.Auth.Enabled && cfg.Auth.JWTSecret == "" {
		secret, err := generateJWTSecret()
		if err != nil {
			return nil, fmt.Errorf("failed to generate JWT secret: %w", err)
		}
		cfg.Auth.JWTSecret = secret

		// Save the generated secret back to config file
		if err := saveJWTSecret(path, secret); err != nil {
			slog.Warn("Could not save JWT secret to config (auth will work but secret will change on restart)", "error", err)
		} else {
			slog.Info("Generated and saved JWT secret to config file")
		}
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}

// generateJWTSecret creates a cryptographically secure random secret
func generateJWTSecret() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(bytes), nil
}

// saveJWTSecret updates the config file with the generated JWT secret
func saveJWTSecret(path, secret string) error {
	// Read existing config
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	content := string(data)

	// Check if auth section exists
	if strings.Contains(content, "auth:") {
		// Update existing auth section
		if strings.Contains(content, "jwt_secret:") {
			// Replace existing jwt_secret line
			lines := strings.Split(content, "\n")
			for i, line := range lines {
				if strings.Contains(line, "jwt_secret:") {
					indent := strings.TrimLeft(line, " \t")
					indentLen := len(line) - len(indent)
					lines[i] = strings.Repeat(" ", indentLen) + "jwt_secret: \"" + secret + "\""
					break
				}
			}
			content = strings.Join(lines, "\n")
		} else {
			// Add jwt_secret after auth:
			content = strings.Replace(content, "auth:", "auth:\n  jwt_secret: \""+secret+"\"", 1)
		}
	} else {
		// Add auth section at the end
		content += "\n\nauth:\n  enabled: true\n  jwt_secret: \"" + secret + "\"\n"
	}

	return os.WriteFile(path, []byte(content), 0644)
}

// Validate checks configuration values for correctness
func (c *Config) Validate() error {
	var errors []string

	// Server validation
	if c.Server.HTTPPort < 1 || c.Server.HTTPPort > 65535 {
		errors = append(errors, fmt.Sprintf("invalid HTTP port: %d (must be 1-65535)", c.Server.HTTPPort))
	}
	if c.Server.WebSocketPort < 1 || c.Server.WebSocketPort > 65535 {
		errors = append(errors, fmt.Sprintf("invalid WebSocket port: %d (must be 1-65535)", c.Server.WebSocketPort))
	}
	if c.Server.HTTPPort == c.Server.WebSocketPort {
		errors = append(errors, "HTTP and WebSocket ports must be different")
	}

	// NATS URL validation
	if c.NATS.URL != "" {
		if !strings.HasPrefix(c.NATS.URL, "nats://") && !strings.HasPrefix(c.NATS.URL, "tls://") {
			errors = append(errors, fmt.Sprintf("invalid NATS URL: %s (must start with nats:// or tls://)", c.NATS.URL))
		}
	}

	// Database validation
	if c.Database.Port < 1 || c.Database.Port > 65535 {
		errors = append(errors, fmt.Sprintf("invalid database port: %d", c.Database.Port))
	}
	validSSLModes := map[string]bool{"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true}
	if !validSSLModes[c.Database.SSLMode] {
		errors = append(errors, fmt.Sprintf("invalid database SSL mode: %s", c.Database.SSLMode))
	}

	// Redis URL validation
	if c.Redis.URL != "" {
		if _, err := url.Parse(c.Redis.URL); err != nil {
			errors = append(errors, fmt.Sprintf("invalid Redis URL: %s", c.Redis.URL))
		}
	}

	// Detection config validation
	if c.Detection.LostThresholdSec < 10 {
		errors = append(errors, fmt.Sprintf("lost_threshold_sec too low: %d (minimum 10)", c.Detection.LostThresholdSec))
	}
	if c.Detection.LostThresholdSec > 3600 {
		errors = append(errors, fmt.Sprintf("lost_threshold_sec too high: %d (maximum 3600)", c.Detection.LostThresholdSec))
	}
	if c.Detection.MaxHistoryHours < 1 || c.Detection.MaxHistoryHours > 87600 {
		errors = append(errors, fmt.Sprintf("max_history_hours out of range: %d (1-87600, max 10 years)", c.Detection.MaxHistoryHours))
	}
	if c.Detection.MaxDisplayedDrones < 10 || c.Detection.MaxDisplayedDrones > 10000 {
		errors = append(errors, fmt.Sprintf("max_displayed_drones out of range: %d (10-10000)", c.Detection.MaxDisplayedDrones))
	}

	if len(errors) > 0 {
		return fmt.Errorf("%s", strings.Join(errors, "; "))
	}
	return nil
}
