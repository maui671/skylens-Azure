package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/K13094/skylens/internal/geo"
)

// localTZ is the timezone for Telegram message timestamps.
// San Juan, PR (AST = UTC-4, no DST).
var localTZ = time.FixedZone("AST", -4*3600)

// TelegramConfig holds bot credentials and alert-type filters
type TelegramConfig struct {
	BotToken  string
	ChatID    string

	NotifyNewDrone  bool
	NotifySpoofing  bool
	NotifyDroneLost bool
	NotifyTapStatus bool
}

// IsConfigured returns true if bot token and chat ID are set
func (c TelegramConfig) IsConfigured() bool {
	return c.BotToken != "" && c.ChatID != ""
}

// Telegram sends alert notifications via the Telegram Bot API
type Telegram struct {
	cfg    TelegramConfig
	client *http.Client

	// Rate limiting
	lastSent time.Time
	mu       sync.Mutex
}

// NewTelegram creates a Telegram notifier
func NewTelegram(cfg TelegramConfig) *Telegram {
	return &Telegram{
		cfg: cfg,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Alert mirrors the api.Alert struct fields we need
type Alert struct {
	ID         string
	Priority   string
	Type       string
	Identifier string
	Message    string
	Timestamp  time.Time
	Latitude   *float64
	Longitude  *float64
	MGRS       string
}

// GetConfig returns the current config (thread-safe)
func (t *Telegram) GetConfig() TelegramConfig {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cfg
}

// UpdateConfig replaces the config at runtime (e.g. from dashboard settings)
func (t *Telegram) UpdateConfig(cfg TelegramConfig) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cfg = cfg
	slog.Info("Telegram config updated",
		"configured", cfg.IsConfigured(),
		"new_drone", cfg.NotifyNewDrone,
		"spoofing", cfg.NotifySpoofing,
		"drone_lost", cfg.NotifyDroneLost,
		"tap_status", cfg.NotifyTapStatus,
	)
}

// SendTest sends a test message to verify the bot is configured correctly
func (t *Telegram) SendTest() error {
	t.mu.Lock()
	cfg := t.cfg
	t.mu.Unlock()

	if !cfg.IsConfigured() {
		return fmt.Errorf("bot token and chat ID are required")
	}
	text := "\xE2\x9C\x85 <b>Skylens</b> \xE2\x80\x94 Test notification\nTelegram integration is working."
	return t.sendMessageWith(cfg.BotToken, cfg.ChatID, text)
}

// Send sends an alert to Telegram if the alert type is enabled
func (t *Telegram) Send(a Alert) {
	t.mu.Lock()
	cfg := t.cfg
	wait := time.Until(t.lastSent.Add(500 * time.Millisecond))
	t.mu.Unlock()

	if wait > 0 {
		time.Sleep(wait)
	}

	if !cfg.IsConfigured() {
		return
	}
	if !shouldSend(cfg, a.Type) {
		return
	}

	t.mu.Lock()
	t.lastSent = time.Now()
	t.mu.Unlock()

	text := formatMessage(a)
	if err := t.sendMessageWith(cfg.BotToken, cfg.ChatID, text); err != nil {
		slog.Warn("Telegram notification failed", "alert_id", a.ID, "error", err)
	}
}

func shouldSend(cfg TelegramConfig, alertType string) bool {
	switch alertType {
	case "new_drone":
		return cfg.NotifyNewDrone
	case "spoof_detected":
		return cfg.NotifySpoofing
	case "drone_lost":
		return cfg.NotifyDroneLost
	case "tap_offline", "tap_online":
		return cfg.NotifyTapStatus
	default:
		return false
	}
}

func formatMessage(a Alert) string {
	pIcon := priorityIcon(a.Priority)
	pLabel := priorityLabel(a.Priority)
	tIcon := typeIcon(a.Type)
	ts := a.Timestamp.In(localTZ).Format("15:04:05")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s <b>%s</b> \u2014 Skylens\n", pIcon, pLabel))
	sb.WriteString(fmt.Sprintf("%s %s\n", tIcon, a.Message))
	if a.Identifier != "" {
		sb.WriteString(fmt.Sprintf("ID: <code>%s</code>\n", a.Identifier))
	}
	if a.Latitude != nil && a.Longitude != nil {
		sb.WriteString(fmt.Sprintf("\U0001F4CD %.4f, %.4f\n", *a.Latitude, *a.Longitude))
		if a.MGRS != "" {
			sb.WriteString(fmt.Sprintf("\U0001F5FA %s\n", a.MGRS))
		}
	}
	sb.WriteString(fmt.Sprintf("\U0001F550 %s", ts))
	return sb.String()
}

func priorityIcon(p string) string {
	switch strings.ToLower(p) {
	case "critical":
		return "\U0001F534" // red circle
	case "high":
		return "\U0001F7E0" // orange circle
	case "medium":
		return "\U0001F7E1" // yellow circle
	case "low", "info":
		return "\U0001F7E2" // green circle
	default:
		return "\u26AA" // white circle
	}
}

func priorityLabel(p string) string {
	switch strings.ToLower(p) {
	case "critical":
		return "CRITICAL"
	case "high":
		return "HIGH"
	case "medium":
		return "ALERT"
	case "low", "info":
		return "INFO"
	default:
		return strings.ToUpper(p)
	}
}

func typeIcon(t string) string {
	switch t {
	case "new_drone":
		return "\u2708\uFE0F" // airplane
	case "drone_lost":
		return "\U0001F4E1" // satellite antenna
	case "spoof_detected":
		return "\u26A0\uFE0F" // warning
	case "tap_offline", "tap_online":
		return "\U0001F50C" // plug
	default:
		return "\u2139\uFE0F" // info
	}
}

type tgRequest struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

func (t *Telegram) sendMessageWith(token, chatID, text string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)

	body, _ := json.Marshal(tgRequest{
		ChatID:    chatID,
		Text:      text,
		ParseMode: "HTML",
	})

	resp, err := t.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var result struct {
			Description string `json:"description"`
		}
		json.NewDecoder(resp.Body).Decode(&result)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, result.Description)
	}
	return nil
}

// =============================================================================
// Military-style Drone Report
// =============================================================================

// DroneReport carries all fields needed to format a rich Telegram notification.
// Flat struct to avoid importing processor (no circular dep).
type DroneReport struct {
	TrackNumber     int
	Designation     string
	DetectionSource string
	SerialNumber    string

	Lat            float64
	Lon            float64
	AltitudeM      float32
	SpeedMS        float32
	VerticalSpeedMS float32
	Heading        float32
	Movement       string

	RSSI              int32
	OperationalStatus string
	FirstSeen         time.Time
	LastSeen          time.Time

	DetectionCount int64

	OperatorLat float64
	OperatorLon float64
	ReportTime  time.Time
}

// SendDroneReport formats and sends a military-style UAV report.
func (t *Telegram) SendDroneReport(r DroneReport) error {
	t.mu.Lock()
	cfg := t.cfg
	wait := time.Until(t.lastSent.Add(500 * time.Millisecond))
	t.mu.Unlock()

	if wait > 0 {
		time.Sleep(wait)
	}

	if !cfg.IsConfigured() {
		return fmt.Errorf("Telegram not configured")
	}

	t.mu.Lock()
	t.lastSent = time.Now()
	t.mu.Unlock()

	text := formatDroneReport(r)
	if err := t.sendMessageWith(cfg.BotToken, cfg.ChatID, text); err != nil {
		slog.Warn("Telegram drone report failed", "track", r.TrackNumber, "error", err)
		return err
	}
	return nil
}

func formatDroneReport(r DroneReport) string {
	var sb strings.Builder

	desig := r.Designation
	if desig == "" || desig == "UNKNOWN" {
		desig = "Unknown"
	}
	src := r.DetectionSource
	if src == "" {
		src = "WiFi"
	}

	sb.WriteString(fmt.Sprintf("<b>[i] UAV INFORMATION REPORT</b>\n\n"))
	sb.WriteString(fmt.Sprintf("<b>TRK-%03d</b> | %s | %s\n", r.TrackNumber, desig, src))
	if r.SerialNumber != "" {
		sb.WriteString(fmt.Sprintf("Serial: <code>%s</code>\n", r.SerialNumber))
	}

	// Position
	sb.WriteString("\n<b>-- POSITION --</b>\n")
	if r.Lat != 0 || r.Lon != 0 {
		mgrs := geo.LatLonToMGRS(r.Lat, r.Lon, 4)
		if mgrs != "" {
			sb.WriteString(fmt.Sprintf("MGRS: %s\n", mgrs))
		}
		sb.WriteString(fmt.Sprintf("DDM: %s, %s\n", latToDDM(r.Lat), lonToDDM(r.Lon)))
	} else {
		sb.WriteString("No GPS position\n")
	}
	altFt := float64(r.AltitudeM) * 3.28084
	spdKts := float64(r.SpeedMS) * 1.94384
	sb.WriteString(fmt.Sprintf("ALT: %.0fm (%.0fft) | SPD: %.1f kts (%.1f m/s)\n",
		r.AltitudeM, altFt, spdKts, r.SpeedMS))
	sb.WriteString(fmt.Sprintf("HDG: %.0f\u00b0 %s | VSPD: %+.1f m/s\n",
		r.Heading, cardinal(r.Heading), r.VerticalSpeedMS))
	mov := r.Movement
	if mov == "" {
		if r.SpeedMS < 0.3 {
			mov = "STATIONARY"
		} else {
			mov = "MOVING"
		}
	}
	sb.WriteString(fmt.Sprintf("Move: %s\n", strings.ToUpper(mov)))

	// Signal
	sb.WriteString("\n<b>-- SIGNAL --</b>\n")
	opSt := r.OperationalStatus
	if opSt == "" || opSt == "OP_STATUS_UNKNOWN" {
		opSt = "Unknown"
	}
	sb.WriteString(fmt.Sprintf("RSSI: %d dBm (%s) | %s\n", r.RSSI, rssiLabel(r.RSSI), opSt))

	// Timing
	sb.WriteString("\n<b>-- TIMING --</b>\n")
	report := r.ReportTime
	if report.IsZero() {
		report = time.Now()
	}
	firstStr := r.FirstSeen.In(localTZ).Format("15:04:05")
	lastStr := r.LastSeen.In(localTZ).Format("15:04:05")
	age := fmtAge(report.Sub(r.FirstSeen))
	sb.WriteString(fmt.Sprintf("First: %s | Last: %s | Age: %s\n", firstStr, lastStr, age))
	if r.DetectionCount > 0 {
		sb.WriteString(fmt.Sprintf("Detections: %d\n", r.DetectionCount))
	}

	// Operator
	if r.OperatorLat != 0 && r.OperatorLon != 0 {
		sb.WriteString("\n<b>-- OPERATOR --</b>\n")
		opMgrs := geo.LatLonToMGRS(r.OperatorLat, r.OperatorLon, 4)
		if opMgrs != "" {
			sb.WriteString(fmt.Sprintf("MGRS: %s\n", opMgrs))
		}
		sb.WriteString(fmt.Sprintf("DDM: %s, %s\n", latToDDM(r.OperatorLat), lonToDDM(r.OperatorLon)))
		if r.Lat != 0 && r.Lon != 0 {
			dist := haversineM(r.Lat, r.Lon, r.OperatorLat, r.OperatorLon)
			distFt := dist * 3.28084
			sb.WriteString(fmt.Sprintf("Distance: %.0fm (%.0fft)\n", dist, distFt))
		}
	}

	// Map links
	if r.Lat != 0 && r.Lon != 0 {
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintf("MAP: <a href=\"https://www.google.com/maps?q=%.6f,%.6f\">UAV</a>", r.Lat, r.Lon))
		if r.OperatorLat != 0 && r.OperatorLon != 0 {
			sb.WriteString(fmt.Sprintf(" | <a href=\"https://www.google.com/maps?q=%.6f,%.6f\">Operator</a>", r.OperatorLat, r.OperatorLon))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(fmt.Sprintf("\n--- %s AST ---", report.In(localTZ).Format("2006-01-02 15:04:05")))

	return sb.String()
}

// latToDDM converts decimal degrees to Degrees Decimal Minutes (DDM) for latitude.
func latToDDM(lat float64) string {
	ns := 'N'
	if lat < 0 {
		ns = 'S'
		lat = -lat
	}
	deg := int(lat)
	min := (lat - float64(deg)) * 60
	return fmt.Sprintf("%d\u00b0%.3f'%c", deg, min, ns)
}

// lonToDDM converts decimal degrees to Degrees Decimal Minutes (DDM) for longitude.
func lonToDDM(lon float64) string {
	ew := 'E'
	if lon < 0 {
		ew = 'W'
		lon = -lon
	}
	deg := int(lon)
	min := (lon - float64(deg)) * 60
	return fmt.Sprintf("%d\u00b0%.3f'%c", deg, min, ew)
}

// cardinal returns the cardinal/intercardinal direction for a heading in degrees.
func cardinal(deg float32) string {
	dirs := []string{"N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE",
		"S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"}
	idx := int(math.Round(float64(deg)/22.5)) % 16
	if idx < 0 {
		idx += 16
	}
	return dirs[idx]
}

// rssiLabel returns a human-readable signal strength label.
func rssiLabel(rssi int32) string {
	switch {
	case rssi >= -50:
		return "Excellent"
	case rssi >= -60:
		return "Strong"
	case rssi >= -70:
		return "Good"
	case rssi >= -80:
		return "Fair"
	case rssi >= -90:
		return "Weak"
	default:
		return "Very Weak"
	}
}

// haversineM returns the distance in meters between two lat/lon points.
func haversineM(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// fmtAge formats a duration as HH:MM:SS.
func fmtAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Seconds())
	h := s / 3600
	m := (s % 3600) / 60
	sec := s % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
}
