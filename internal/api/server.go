package api

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/K13094/skylens/internal/auth"
	"github.com/K13094/skylens/internal/geo"
	"github.com/K13094/skylens/internal/intel"
	"github.com/K13094/skylens/internal/notify"
	"github.com/K13094/skylens/internal/processor"
	"github.com/K13094/skylens/internal/tak"
	"github.com/K13094/skylens/internal/receiver"
	"github.com/K13094/skylens/internal/storage"
)

// Rate limiter settings
const (
	rateLimitRequests = 100 // requests per window
	rateLimitWindow   = time.Second
	wsPingInterval    = 30 * time.Second
	wsPongTimeout     = 10 * time.Second
)

// WebSocket batching settings
const (
	wsBatchInterval = 16 * time.Millisecond // Flush every 16ms (one browser frame) — batch dedup keeps payload small
	wsBatchMaxSize  = 50                     // Max events before forced flush
)

// wsBatcher collects events and flushes them periodically as batched messages
type wsBatcher struct {
	mu      sync.Mutex
	events  []map[string]interface{}
	flushCh chan struct{} // Signal to flush immediately
}

// RateLimiter implements a simple sliding window rate limiter
type RateLimiter struct {
	mu       sync.Mutex
	clients  map[string]*clientLimit
	requests int
	window   time.Duration
	done     chan struct{}
}

type clientLimit struct {
	count    int
	windowStart time.Time
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(requests int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		clients:  make(map[string]*clientLimit),
		requests: requests,
		window:   window,
		done:     make(chan struct{}),
	}
	// Cleanup goroutine (stops when Close is called)
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rl.cleanup()
			case <-rl.done:
				return
			}
		}
	}()
	return rl
}

// Allow checks if a request from the given IP is allowed
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	client, exists := rl.clients[ip]

	if !exists {
		rl.clients[ip] = &clientLimit{count: 1, windowStart: now}
		return true
	}

	// Reset window if expired
	if now.Sub(client.windowStart) > rl.window {
		client.count = 1
		client.windowStart = now
		return true
	}

	// Check limit
	if client.count >= rl.requests {
		return false
	}

	client.count++
	return true
}

// Close stops the background cleanup goroutine
func (rl *RateLimiter) Close() {
	close(rl.done)
}

// cleanup removes stale entries
func (rl *RateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	for ip, client := range rl.clients {
		if now.Sub(client.windowStart) > rl.window*2 {
			delete(rl.clients, ip)
		}
	}
}

//go:embed dashboard/static
var staticFiles embed.FS

// ServerConfig holds server settings
type ServerConfig struct {
	HTTPSPort    int
	TLSCertFile  string
	TLSKeyFile   string
	APIKey         string   // Optional API key for authentication
	AllowedOrigins []string // Allowed WebSocket origins (empty = same-origin only)

	// Auth settings
	AuthEnabled   bool
	JWTSecret     string
	SecureCookies bool
	JWTExpiry     time.Duration
	RefreshExpiry time.Duration
	SessionExpiry time.Duration
}

// wsClient wraps a WebSocket connection with a write mutex
type wsClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

// Server handles HTTP and WebSocket connections
type Server struct {
	cfg         ServerConfig
	state       *processor.StateManager
	store       *storage.Store
	receiver    *receiver.Receiver // NATS receiver for sending commands
	upgrader    websocket.Upgrader
	wsClients   map[*websocket.Conn]*wsClient
	wsMu        sync.RWMutex
	wsBatcher   *wsBatcher // Batches events for efficient WebSocket broadcast
	rateLimiter *RateLimiter
	httpServer  *http.Server
	activeConns int64     // atomic counter for graceful shutdown
	startTime   time.Time // server start time for uptime
	authInt     *AuthIntegration // Auth integration (nil if auth disabled)
	alerted     map[string]struct{} // Track alerts already sent (e.g., spoof alerts)
	tapStatus   map[string]string   // Track tap status for change detection
	alertedMu   sync.Mutex          // Protects alerted and tapStatus maps
	shutdownCh  chan struct{}        // Closed on Shutdown() to stop background goroutines
	eventCh     chan processor.StateEvent // State event channel (for cleanup on shutdown)
	wsTickets   map[string]*wsTicket // One-time WebSocket auth tickets
	wsTicketMu  sync.Mutex
}

// wsTicket is a one-time auth ticket for WebSocket upgrades.
// Prevents JWT from leaking in URL query params / server logs / referrer headers.
type wsTicket struct {
	userID   int
	username string
	role     string
	expires  time.Time
}

// NewServer creates a new API server
func NewServer(cfg ServerConfig, state *processor.StateManager, store *storage.Store, recv *receiver.Receiver) *Server {
	s := &Server{
		cfg:         cfg,
		state:       state,
		store:       store,
		receiver:    recv,
		wsClients:   make(map[*websocket.Conn]*wsClient),
		wsBatcher: &wsBatcher{
			events:  make([]map[string]interface{}, 0, wsBatchMaxSize),
			flushCh: make(chan struct{}, 1),
		},
		rateLimiter: NewRateLimiter(rateLimitRequests, rateLimitWindow),
		startTime:   time.Now(),
		alerted:     make(map[string]struct{}),
		tapStatus:   make(map[string]string),
		shutdownCh:  make(chan struct{}),
		wsTickets:   make(map[string]*wsTicket),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true // No origin header (same-origin request)
				}

				// Check against allowed origins
				if len(cfg.AllowedOrigins) > 0 {
					for _, allowed := range cfg.AllowedOrigins {
						if allowed == "*" || allowed == origin {
							return true
						}
					}
					return false
				}

				// Default: validate same-origin
				host := r.Host
				if host == "" {
					return false
				}
				// Extract host from origin URL
				if len(origin) > 7 && origin[:7] == "http://" {
					origin = origin[7:]
				} else if len(origin) > 8 && origin[:8] == "https://" {
					origin = origin[8:]
				}
				// Compare host (strip port if needed)
				originHost := origin
				if idx := len(originHost) - 1; idx > 0 {
					for i := 0; i < len(originHost); i++ {
						if originHost[i] == '/' {
							originHost = originHost[:i]
							break
						}
					}
				}
				hostOnly := host
				for i := 0; i < len(hostOnly); i++ {
					if hostOnly[i] == ':' {
						hostOnly = hostOnly[:i]
						break
					}
				}
				for i := 0; i < len(originHost); i++ {
					if originHost[i] == ':' {
						originHost = originHost[:i]
						break
					}
				}
				return hostOnly == originHost
			},
			ReadBufferSize:    1024,
			WriteBufferSize:   32768,
			EnableCompression: true,
		},
	}

	// Subscribe to state events for WebSocket broadcasting
	s.eventCh = make(chan processor.StateEvent, 2000)
	state.Subscribe(s.eventCh)
	go s.collectEvents(s.eventCh)
	go s.flushBatchedEvents()

	// Enable alert persistence via the store
	alertStore = store

	// Restore alerts from database
	if store != nil {
		if rows, err := store.LoadAlerts(maxAlerts); err != nil {
			slog.Warn("Failed to load alerts from database", "error", err)
		} else if len(rows) > 0 {
			alertMu.Lock()
			for _, r := range rows {
				alerts = append(alerts, Alert{
					ID:        r.ID,
					Priority:  normalizePriority(r.Priority),
					Type:      r.AlertType,
					Identifier: r.Identifier,
					Message:   r.Message,
					Timestamp: r.CreatedAt,
					Acked:     r.Acked,
					ExpiresAt: r.ExpiresAt,
				})
			}
			// Rows come newest-first from DB; reverse so oldest is first (matching in-memory order)
			for i, j := 0, len(alerts)-1; i < j; i, j = i+1, j-1 {
				alerts[i], alerts[j] = alerts[j], alerts[i]
			}
			alertMu.Unlock()
			slog.Info("Restored alerts from database", "count", len(rows))
		}
	}

	// Start cleanup goroutines (all listen to shutdownCh for clean exit)
	startAlertCleanup(s.shutdownCh)
	startSuspectCleanup(s.shutdownCh)
	go s.cleanupWSTickets()

	return s
}

// Start runs the HTTP and WebSocket servers
func (s *Server) Start(ctx context.Context) {
	// HTTP server with rate limiting middleware
	httpMux := http.NewServeMux()
	s.registerRoutes(httpMux)

	// Wrap with rate limiting, API key auth, security headers, gzip, and connection tracking
	var handler http.Handler = httpMux
	handler = s.connectionTrackingMiddleware(handler)
	if s.cfg.APIKey != "" {
		handler = s.apiKeyMiddleware(handler)
		slog.Info("API key authentication enabled")
	}
	handler = s.securityHeadersMiddleware(handler)
	handler = gzipMiddleware(handler)
	handler = s.rateLimitMiddleware(handler)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.cfg.HTTPSPort),
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("HTTPS server starting", "port", s.cfg.HTTPSPort, "cert", s.cfg.TLSCertFile, "key", s.cfg.TLSKeyFile)
		if err := s.httpServer.ListenAndServeTLS(s.cfg.TLSCertFile, s.cfg.TLSKeyFile); err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
		}
	}()

	// WebSocket server (separate port for real-time)

	<-ctx.Done()
	s.gracefulShutdown()
}

// rateLimitMiddleware applies rate limiting to requests
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip rate limiting for health checks
		if r.URL.Path == "/health" || r.URL.Path == "/ready" {
			next.ServeHTTP(w, r)
			return
		}

		// Extract client IP
		ip := r.RemoteAddr
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			ip = strings.Split(xff, ",")[0]
		}

		if !s.rateLimiter.Allow(ip) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// gzipResponseWriter wraps http.ResponseWriter with gzip compression
type gzipResponseWriter struct {
	http.ResponseWriter
	Writer *gzip.Writer
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

// gzipMiddleware compresses API JSON responses (typically 70%+ reduction)
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only compress /api/ routes (skip static assets, SSE, health checks)
		if !strings.HasPrefix(r.URL.Path, "/api/") ||
			r.URL.Path == "/api/events" { // SSE uses chunked encoding
			next.ServeHTTP(w, r)
			return
		}

		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		gz, _ := gzip.NewWriterLevel(w, flate.BestSpeed)
		defer gz.Close()

		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")

		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, Writer: gz}, r)
	})
}

// apiKeyMiddleware validates the X-API-Key header for protected routes
func (s *Server) apiKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Exempt routes from authentication:
		// - Health check endpoints (for container orchestration)
		// - Static files (dashboard assets)
		// - WebSocket upgrade (uses separate auth if needed)
		if path == "/health" || path == "/ready" ||
			path == "/api/health" || path == "/api/ready" ||
			strings.HasPrefix(path, "/css/") ||
			strings.HasPrefix(path, "/js/") ||
			strings.HasPrefix(path, "/img/") ||
			path == "/manifest.json" ||
			path == "/service-worker.js" ||
			path == "/" ||
			path == "/airspace" ||
			path == "/fleet" ||
			path == "/taps" ||
			path == "/alerts" ||
			path == "/settings" ||
			path == "/system" ||
			path == "/analytics" {
			next.ServeHTTP(w, r)
			return
		}

		// Check for API key in header
		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			// Also check query parameter for WebSocket connections
			apiKey = r.URL.Query().Get("api_key")
		}

		if apiKey == "" {
			s.setCORSHeaders(w, r)
			w.Header().Set("WWW-Authenticate", `API-Key realm="skylens"`)
			http.Error(w, `{"error":"API key required","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
			return
		}

		if apiKey != s.cfg.APIKey {
			s.setCORSHeaders(w, r)
			slog.Warn("Invalid API key attempt", "remote", r.RemoteAddr, "path", path)
			http.Error(w, `{"error":"Invalid API key","code":"FORBIDDEN"}`, http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// connectionTrackingMiddleware tracks active connections for graceful shutdown
func (s *Server) connectionTrackingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&s.activeConns, 1)
		defer atomic.AddInt64(&s.activeConns, -1)
		next.ServeHTTP(w, r)
	})
}

// securityHeadersMiddleware adds security headers to all HTTP responses.
// CSP prevents XSS/injection; other headers harden the response surface.
func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	// Build CSP once at startup
	csp := strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'unsafe-inline' https://unpkg.com",
		"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com https://unpkg.com",
		"img-src 'self' data: https://*.tile.openstreetmap.org https://server.arcgisonline.com https://*.basemaps.cartocdn.com",
		"font-src 'self' https://fonts.gstatic.com",
		"connect-src 'self' ws: wss:",
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
	}, "; ")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

// issueWSTicket creates a one-time ticket for WebSocket authentication.
// The ticket expires after 30 seconds and can only be used once.
func (s *Server) issueWSTicket(userID int, username, role string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	ticket := hex.EncodeToString(b)

	s.wsTicketMu.Lock()
	s.wsTickets[ticket] = &wsTicket{
		userID:   userID,
		username: username,
		role:     role,
		expires:  time.Now().Add(30 * time.Second),
	}
	s.wsTicketMu.Unlock()

	return ticket, nil
}

// consumeWSTicket validates and consumes a one-time WebSocket ticket.
// Returns the ticket info on success, nil if invalid/expired/already-used.
func (s *Server) consumeWSTicket(ticket string) *wsTicket {
	s.wsTicketMu.Lock()
	defer s.wsTicketMu.Unlock()

	t, ok := s.wsTickets[ticket]
	if !ok {
		return nil
	}
	delete(s.wsTickets, ticket) // one-time use

	if time.Now().After(t.expires) {
		return nil
	}
	return t
}

// cleanupWSTickets removes expired tickets periodically.
func (s *Server) cleanupWSTickets() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ticker.C:
			now := time.Now()
			s.wsTicketMu.Lock()
			for k, t := range s.wsTickets {
				if now.After(t.expires) {
					delete(s.wsTickets, k)
				}
			}
			s.wsTicketMu.Unlock()
		}
	}
}

// handleWSTicket issues a one-time WebSocket auth ticket.
// POST /api/auth/ws-ticket (requires authentication)
func (s *Server) handleWSTicket(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Extract user from auth context (set by RequireAuth middleware)
	user := auth.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
		return
	}

	ticket, err := s.issueWSTicket(user.ID, user.Username, user.RoleName)
	if err != nil {
		slog.Error("Failed to generate WS ticket", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	s.writeJSON(w, map[string]string{"ticket": ticket})
}

// gracefulShutdown shuts down servers gracefully
func (s *Server) gracefulShutdown() {
	slog.Info("Starting graceful shutdown...")

	// Close all WebSocket connections first
	s.wsMu.Lock()
	for conn, client := range s.wsClients {
		client.writeMu.Lock()
		conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down"),
			time.Now().Add(time.Second),
		)
		conn.Close()
		client.writeMu.Unlock()
	}
	s.wsClients = make(map[*websocket.Conn]*wsClient)
	s.wsMu.Unlock()

	// Unsubscribe event channel so StateManager stops broadcasting to it,
	// then close the channel so collectEvents exits cleanly.
	if s.eventCh != nil {
		s.state.Unsubscribe(s.eventCh)
		close(s.eventCh)
	}

	// Stop rate limiter cleanup goroutine
	s.rateLimiter.Close()

	// Stop auth cleanup goroutines
	if s.authInt != nil {
		s.authInt.Close()
	}

	// Create shutdown context with timeout
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shutdown HTTP server
	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Warn("HTTP server shutdown error", "error", err)
	}

	// Wait for active connections to drain (up to 5 seconds)
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&s.activeConns) > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}

	if conns := atomic.LoadInt64(&s.activeConns); conns > 0 {
		slog.Warn("Shutdown with active connections", "count", conns)
	} else {
		slog.Info("All connections drained")
	}
}

// registerRoutes sets up HTTP routes
func (s *Server) registerRoutes(mux *http.ServeMux) {
	// Health check endpoints (for container orchestration) - always public
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/ready", s.handleReady)
	mux.HandleFunc("/metrics", s.handleMetrics) // Prometheus-compatible metrics

	// Authentication routes
	s.authInt = s.SetupAuth()
	s.RegisterAuthRoutes(mux, s.authInt)

	// WebSocket ticket endpoint (protected - requires auth)
	if s.authInt != nil {
		mux.Handle("/api/auth/ws-ticket", s.authInt.middleware.RequireAuth(
			http.HandlerFunc(s.handleWSTicket)))
	}

	// WebSocket endpoint
	mux.HandleFunc("/ws", s.handleWebSocket)

	// Helper to wrap handlers with auth middleware when auth is enabled
	protect := func(h http.HandlerFunc) http.Handler {
		if s.authInt != nil {
			return s.authInt.middleware.RequireAuth(http.HandlerFunc(h))
		}
		return http.HandlerFunc(h)
	}

	// Core API routes - protected when auth enabled
	mux.Handle("/api/status", protect(s.handleStatus))
	mux.Handle("/api/drones", protect(s.handleDrones))
	mux.Handle("/api/drones/", protect(s.handleDroneByID))
	mux.Handle("/api/taps", protect(s.handleTaps))
	mux.Handle("/api/stats", protect(s.handleStats))

	// Dashboard-compatible routes - protected
	mux.Handle("/api/threat", protect(s.handleThreat))
	mux.Handle("/api/fleet", protect(s.handleFleet))
	mux.Handle("/api/alerts", protect(s.handleAlerts))
	mux.Handle("/api/alert/", protect(s.handleAlertByID))     // /api/alert/{id}/ack
	mux.Handle("/api/alerts/", protect(s.handleAlertsAction)) // /api/alerts/ack-all, /api/alerts/clear
	mux.Handle("/api/system/stats", protect(s.handleSystemStats))
	mux.Handle("/api/events", protect(s.handleSSE))

	// UAV management routes - protected
	mux.Handle("/api/uav/", protect(s.handleUAVAction))       // /api/uav/{id}/hide, /api/uav/{id}/delete, /api/uav/{id}/history
	mux.Handle("/api/uavs/hide-lost", protect(s.handleHideLost))
	mux.Handle("/api/uavs/unhide-all", protect(s.handleUnhideAll))

	// TAP command routes - protected
	mux.Handle("/api/tap/", protect(s.handleTapCommand))           // /api/tap/{id}/ping, /api/tap/{id}/restart, /api/tap/{id}/command
	mux.Handle("/api/taps/broadcast/", protect(s.handleBroadcast)) // /api/taps/broadcast/{command}

	// Analytics routes - protected
	mux.Handle("/api/detections/history", protect(s.handleDetectionsHistory))

	// Trail routes (GPS flight paths) - protected
	mux.Handle("/api/trails", protect(s.handleTrails))     // GET: all active trails
	mux.Handle("/api/trails/", protect(s.handleTrailByID)) // GET: /api/trails/{drone_id}

	// Suspect and signature learning routes - protected
	mux.Handle("/api/suspects", protect(s.handleSuspects))       // GET: list all suspects
	mux.Handle("/api/suspects/", protect(s.handleSuspectAction)) // POST: /api/suspects/{mac}/confirm, /api/suspects/{mac}/dismiss
	mux.Handle("/api/signatures", protect(s.handleSignatures))   // GET: list learned signatures

	// User preferences (per-account settings persistence)
	mux.Handle("/api/user/preferences", protect(s.handleUserPreferences))

	// Telegram test route - protected
	mux.Handle("/api/telegram/test", protect(s.handleTelegramTest))
	mux.Handle("/api/telegram/status", protect(s.handleTelegramStatus))

	// TAK Server routes - protected
	mux.Handle("/api/tak/status", protect(s.handleTAKStatus))
	mux.Handle("/api/tak/test", protect(s.handleTAKTest))
	mux.Handle("/api/tak/upload-cert", protect(s.handleTAKUploadCert))

	// Intel update route - protected
	mux.Handle("/api/intel/update", protect(s.handleIntelUpdate))

	// Test routes - protected (dangerous operations)
	mux.Handle("/api/test/drone", protect(s.handleTestDrone))
	mux.Handle("/api/test/tap", protect(s.handleTestTap))
	mux.Handle("/api/test/simulate", protect(s.handleSimulate))
	mux.Handle("/api/test/clear", protect(s.handleClearData)) // alias for settings page
	mux.Handle("/api/data/clear", protect(s.handleClearData))

	// Serve static dashboard files
	staticFS, err := fs.Sub(staticFiles, "dashboard/static")
	if err != nil {
		slog.Warn("Failed to load embedded static files", "error", err)
		// Fallback to local files - still need explicit page routes
	}

	// Page routes (for HTML pages without .html extension)
	pages := map[string]string{
		"/airspace":  "airspace.html",
		"/fleet":     "fleet.html",
		"/taps":      "taps.html",
		"/alerts":    "alerts.html",
		"/settings":  "settings.html",
		"/system":    "system.html",
		"/analytics": "analytics.html",
		"/login":     "login.html",
		"/admin":     "admin.html",
		"/profile":   "profile.html",
		"/tak":       "tak.html",
	}

	for route, file := range pages {
		route, file := route, file // Capture for closure
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			s.serveStaticFile(w, r, file)
		})
	}

	// Serve static files (CSS, JS, images) with cache headers
	cacheAssets := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=86400, stale-while-revalidate=3600")
			h.ServeHTTP(w, r)
		})
	}
	if staticFS != nil {
		fs := http.FileServer(http.FS(staticFS))
		mux.Handle("/css/", cacheAssets(fs))
		mux.Handle("/js/", cacheAssets(fs))
		mux.Handle("/images/", cacheAssets(fs))
		mux.Handle("/", fs)
	} else {
		fileServer := http.FileServer(http.Dir("internal/api/dashboard/static"))
		mux.Handle("/css/", cacheAssets(fileServer))
		mux.Handle("/js/", cacheAssets(fileServer))
		mux.Handle("/images/", cacheAssets(fileServer))
		mux.Handle("/", fileServer)
	}
}

var serverStartTime = time.Now()

// handleHealth returns basic health status for liveness probes
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	uptime := time.Since(serverStartTime)

	// Check database connectivity
	dbStatus := "ok"
	if s.store != nil && !s.store.IsHealthy() {
		dbStatus = "degraded"
	}

	health := map[string]interface{}{
		"status":      "ok",
		"uptime_sec":  uptime.Seconds(),
		"uptime":      formatUptime(uptime),
		"db":          dbStatus,
		"timestamp":   time.Now().Format(time.RFC3339),
	}

	s.writeJSON(w, health)
}

// handleReady returns readiness status for readiness probes
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	// Check all required dependencies
	ready := true
	checks := make(map[string]string)

	// Check state manager
	if s.state == nil {
		ready = false
		checks["state"] = "not initialized"
	} else {
		checks["state"] = "ok"
	}

	// Check database (optional but report status)
	if s.store != nil {
		if s.store.IsHealthy() {
			checks["database"] = "ok"
		} else {
			checks["database"] = "degraded"
			// Database is optional, don't fail readiness
		}
	} else {
		checks["database"] = "disabled"
	}

	status := "ready"
	statusCode := http.StatusOK
	if !ready {
		status = "not_ready"
		statusCode = http.StatusServiceUnavailable
	}

	result := map[string]interface{}{
		"status": status,
		"checks": checks,
	}

	w.WriteHeader(statusCode)
	s.writeJSON(w, result)
}

// handleMetrics returns Prometheus-compatible metrics
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := s.state.GetStats()
	drones := s.state.GetAllDrones()
	taps := s.state.GetAllTaps()
	uptime := time.Since(serverStartTime).Seconds()

	// Count drones by status (exclude controllers)
	activeCount := 0
	lostCount := 0
	controllerCount := 0
	for _, d := range drones {
		if d.IsController {
			controllerCount++
			continue
		}
		if d.Status == "active" {
			activeCount++
		} else if d.Status == "lost" {
			lostCount++
		}
	}
	droneTotal := activeCount + lostCount

	// Count connected taps
	connectedTaps := 0
	for _, t := range taps {
		if t.Status == "online" {
			connectedTaps++
		}
	}

	// WebSocket client count
	s.wsMu.RLock()
	wsClients := len(s.wsClients)
	s.wsMu.RUnlock()

	// Active HTTP connections
	httpConns := atomic.LoadInt64(&s.activeConns)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	// Output Prometheus-compatible metrics
	fmt.Fprintf(w, "# HELP skylens_uptime_seconds Time since server started\n")
	fmt.Fprintf(w, "# TYPE skylens_uptime_seconds gauge\n")
	fmt.Fprintf(w, "skylens_uptime_seconds %.2f\n\n", uptime)

	fmt.Fprintf(w, "# HELP skylens_drones_total Total number of tracked drones (excluding controllers)\n")
	fmt.Fprintf(w, "# TYPE skylens_drones_total gauge\n")
	fmt.Fprintf(w, "skylens_drones_total %d\n\n", droneTotal)

	fmt.Fprintf(w, "# HELP skylens_controllers_total Total number of tracked controllers\n")
	fmt.Fprintf(w, "# TYPE skylens_controllers_total gauge\n")
	fmt.Fprintf(w, "skylens_controllers_total %d\n\n", controllerCount)

	fmt.Fprintf(w, "# HELP skylens_drones_active Number of active drones\n")
	fmt.Fprintf(w, "# TYPE skylens_drones_active gauge\n")
	fmt.Fprintf(w, "skylens_drones_active %d\n\n", activeCount)

	fmt.Fprintf(w, "# HELP skylens_drones_lost Number of lost drones\n")
	fmt.Fprintf(w, "# TYPE skylens_drones_lost gauge\n")
	fmt.Fprintf(w, "skylens_drones_lost %d\n\n", lostCount)

	fmt.Fprintf(w, "# HELP skylens_taps_total Total number of registered taps\n")
	fmt.Fprintf(w, "# TYPE skylens_taps_total gauge\n")
	fmt.Fprintf(w, "skylens_taps_total %d\n\n", len(taps))

	fmt.Fprintf(w, "# HELP skylens_taps_connected Number of connected taps\n")
	fmt.Fprintf(w, "# TYPE skylens_taps_connected gauge\n")
	fmt.Fprintf(w, "skylens_taps_connected %d\n\n", connectedTaps)

	// Get total detections from stats map
	totalDetections := int64(0)
	if td, ok := stats["total_detections"]; ok {
		switch v := td.(type) {
		case int64:
			totalDetections = v
		case int:
			totalDetections = int64(v)
		case float64:
			totalDetections = int64(v)
		}
	}

	fmt.Fprintf(w, "# HELP skylens_detections_total Total detections processed\n")
	fmt.Fprintf(w, "# TYPE skylens_detections_total counter\n")
	fmt.Fprintf(w, "skylens_detections_total %d\n\n", totalDetections)

	fmt.Fprintf(w, "# HELP skylens_websocket_clients Number of connected WebSocket clients\n")
	fmt.Fprintf(w, "# TYPE skylens_websocket_clients gauge\n")
	fmt.Fprintf(w, "skylens_websocket_clients %d\n\n", wsClients)

	fmt.Fprintf(w, "# HELP skylens_http_connections Active HTTP connections\n")
	fmt.Fprintf(w, "# TYPE skylens_http_connections gauge\n")
	fmt.Fprintf(w, "skylens_http_connections %d\n\n", httpConns)

	// Alert counts
	alertMu.Lock()
	unackedCount := 0
	criticalCount := 0
	for _, a := range alerts {
		if !a.Acked {
			unackedCount++
		}
		if a.Priority == "critical" {
			criticalCount++
		}
	}
	totalAlerts := len(alerts)
	alertMu.Unlock()

	fmt.Fprintf(w, "# HELP skylens_alerts_total Total alerts in history\n")
	fmt.Fprintf(w, "# TYPE skylens_alerts_total gauge\n")
	fmt.Fprintf(w, "skylens_alerts_total %d\n\n", totalAlerts)

	fmt.Fprintf(w, "# HELP skylens_alerts_unacknowledged Unacknowledged alerts\n")
	fmt.Fprintf(w, "# TYPE skylens_alerts_unacknowledged gauge\n")
	fmt.Fprintf(w, "skylens_alerts_unacknowledged %d\n\n", unackedCount)

	fmt.Fprintf(w, "# HELP skylens_alerts_critical Critical severity alerts\n")
	fmt.Fprintf(w, "# TYPE skylens_alerts_critical gauge\n")
	fmt.Fprintf(w, "skylens_alerts_critical %d\n\n", criticalCount)

	// Database worker metrics
	if s.store != nil {
		dbMetrics := s.store.GetMetrics()

		fmt.Fprintf(w, "# HELP skylens_db_detections_saved_total Total detections written to database\n")
		fmt.Fprintf(w, "# TYPE skylens_db_detections_saved_total counter\n")
		fmt.Fprintf(w, "skylens_db_detections_saved_total %d\n\n", dbMetrics.DetectionsSaved)

		fmt.Fprintf(w, "# HELP skylens_db_drones_saved_total Total drone upserts to database\n")
		fmt.Fprintf(w, "# TYPE skylens_db_drones_saved_total counter\n")
		fmt.Fprintf(w, "skylens_db_drones_saved_total %d\n\n", dbMetrics.DronesSaved)

		fmt.Fprintf(w, "# HELP skylens_db_fp_cache_hits False positive cache hits\n")
		fmt.Fprintf(w, "# TYPE skylens_db_fp_cache_hits counter\n")
		fmt.Fprintf(w, "skylens_db_fp_cache_hits %d\n\n", dbMetrics.FalsePositiveHits)

		fmt.Fprintf(w, "# HELP skylens_db_fp_cache_misses False positive cache misses\n")
		fmt.Fprintf(w, "# TYPE skylens_db_fp_cache_misses counter\n")
		fmt.Fprintf(w, "skylens_db_fp_cache_misses %d\n\n", dbMetrics.FalsePositiveMisses)

		fmt.Fprintf(w, "# HELP skylens_db_cleanup_deleted_total Total rows deleted in cleanup\n")
		fmt.Fprintf(w, "# TYPE skylens_db_cleanup_deleted_total counter\n")
		fmt.Fprintf(w, "skylens_db_cleanup_deleted_total %d\n\n", dbMetrics.CleanupDeleted)

		fmt.Fprintf(w, "# HELP skylens_db_query_errors_total Total database query errors\n")
		fmt.Fprintf(w, "# TYPE skylens_db_query_errors_total counter\n")
		fmt.Fprintf(w, "skylens_db_query_errors_total %d\n\n", dbMetrics.QueryErrors)

		if !dbMetrics.LastCleanupTime.IsZero() {
			fmt.Fprintf(w, "# HELP skylens_db_last_cleanup_duration_seconds Duration of last cleanup run\n")
			fmt.Fprintf(w, "# TYPE skylens_db_last_cleanup_duration_seconds gauge\n")
			fmt.Fprintf(w, "skylens_db_last_cleanup_duration_seconds %.3f\n\n", dbMetrics.LastCleanupDuration)

			fmt.Fprintf(w, "# HELP skylens_db_last_cleanup_timestamp_seconds Unix timestamp of last cleanup\n")
			fmt.Fprintf(w, "# TYPE skylens_db_last_cleanup_timestamp_seconds gauge\n")
			fmt.Fprintf(w, "skylens_db_last_cleanup_timestamp_seconds %d\n\n", dbMetrics.LastCleanupTime.Unix())
		}
	}

	// Pipeline health metrics
	if s.receiver != nil {
		pStats := s.receiver.GetPipelineStats()
		fmt.Fprintf(w, "# HELP skylens_pipeline_db_queue_depth Current DB job queue depth\n")
		fmt.Fprintf(w, "# TYPE skylens_pipeline_db_queue_depth gauge\n")
		fmt.Fprintf(w, "skylens_pipeline_db_queue_depth %v\n\n", pStats["db_queue_depth"])

		fmt.Fprintf(w, "# HELP skylens_pipeline_db_save_errors_total DB save failures after retries\n")
		fmt.Fprintf(w, "# TYPE skylens_pipeline_db_save_errors_total counter\n")
		fmt.Fprintf(w, "skylens_pipeline_db_save_errors_total %v\n\n", pStats["db_save_errors"])

		fmt.Fprintf(w, "# HELP skylens_pipeline_db_queue_drops_total Detections dropped due to full queue\n")
		fmt.Fprintf(w, "# TYPE skylens_pipeline_db_queue_drops_total counter\n")
		fmt.Fprintf(w, "skylens_pipeline_db_queue_drops_total %v\n\n", pStats["db_queue_drops"])

		fmt.Fprintf(w, "# HELP skylens_pipeline_detections_received_total Detections received from NATS\n")
		fmt.Fprintf(w, "# TYPE skylens_pipeline_detections_received_total counter\n")
		fmt.Fprintf(w, "skylens_pipeline_detections_received_total %v\n\n", pStats["detections_received"])

		fmt.Fprintf(w, "# HELP skylens_pipeline_heartbeats_received_total Heartbeats received from NATS\n")
		fmt.Fprintf(w, "# TYPE skylens_pipeline_heartbeats_received_total counter\n")
		fmt.Fprintf(w, "skylens_pipeline_heartbeats_received_total %v\n\n", pStats["heartbeats_received"])
	}
}

// handleStatus returns current system status (dashboard-compatible)
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	stats := s.state.GetStats()
	drones := s.state.GetAllDrones()
	taps := s.state.GetAllTaps()

	// Enrich drones with model and distance estimate if missing
	for _, d := range drones {
		// Model from serial
		if d.Model == "" && d.SerialNumber != "" {
			d.Model = intel.GetModelFromSerial(d.SerialNumber)
		}
		// Distance estimate from RSSI if not already calculated (uses per-TAP calibration + environment)
		if d.DistanceEstM == 0 && d.RSSI != 0 && d.RSSI > -120 && d.RSSI < -20 {
			est := intel.EstimateDistanceWithBounds(float64(d.RSSI), d.Model, d.TapID)
			if est.DistanceM > 0 {
				d.DistanceEstM = est.DistanceM
			}
		}
		// Ensure detection source is set
		if d.DetectionSource == "" || d.DetectionSource == "SOURCE_UNKNOWN" {
			d.DetectionSource = "WiFi RemoteID"
		}
	}

	// Calculate uptime string
	uptime := time.Since(serverStartTime)
	uptimeStr := formatUptime(uptime)

	// Get alerts
	alertMu.Lock()
	alertsCopy := make([]Alert, len(alerts))
	copy(alertsCopy, alerts)
	alertMu.Unlock()

	response := map[string]interface{}{
		"connected":      true,
		"stats":          stats,
		"uavs":           drones,
		"taps":           taps,
		"alerts_history": alertsCopy,
		"uptime":         uptimeStr,
		"node_name":      "skylens-node",
		"version":        "0.1.0",
		"intel_version":  intel.GetDroneModelsVersion("internal/intel/drone_models.json"),
	}

	s.writeJSON(w, response)
}

func formatUptime(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// handleDrones returns all drones
func (s *Server) handleDrones(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	drones := s.state.GetAllDrones()

	// Enrich drones with model and distance estimate if missing
	for _, d := range drones {
		// Model from serial
		if d.Model == "" && d.SerialNumber != "" {
			d.Model = intel.GetModelFromSerial(d.SerialNumber)
		}
		// Distance estimate from RSSI if not already calculated (uses per-TAP calibration + environment)
		if d.DistanceEstM == 0 && d.RSSI != 0 && d.RSSI > -120 && d.RSSI < -20 {
			est := intel.EstimateDistanceWithBounds(float64(d.RSSI), d.Model, d.TapID)
			if est.DistanceM > 0 {
				d.DistanceEstM = est.DistanceM
			}
		}
		// Ensure detection source is set
		if d.DetectionSource == "" || d.DetectionSource == "SOURCE_UNKNOWN" {
			d.DetectionSource = "WiFi RemoteID"
		}
	}

	s.writeJSON(w, drones)
}

// handleDroneByID handles single drone operations
func (s *Server) handleDroneByID(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	// Extract ID from path: /api/drones/{id}
	id := r.URL.Path[len("/api/drones/"):]
	if id == "" {
		http.Error(w, "Missing drone ID", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case "GET":
		drone, ok := s.state.GetDrone(id)
		if !ok {
			http.Error(w, "Drone not found", http.StatusNotFound)
			return
		}
		s.writeJSON(w, drone)

	case "DELETE":
		if drone, ok := s.state.GetDrone(id); ok {
			slog.Warn("Drone deleted via API",
				"identifier", id,
				"track_number", drone.TrackNumber,
				"manufacturer", drone.Manufacturer,
				"model", drone.Model,
				"designation", drone.Designation,
				"remote_addr", r.RemoteAddr,
			)
		}
		if s.state.DeleteDrone(id) {
			s.writeJSON(w, map[string]bool{"ok": true})
		} else {
			http.Error(w, "Drone not found", http.StatusNotFound)
		}

	case "PATCH", "POST":
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		if tag, ok := body["tag"]; ok {
			s.state.SetDroneTag(id, tag)
			if s.store != nil {
				if err := s.store.UpdateDroneTag(id, tag); err != nil {
					slog.Warn("Failed to persist tag to DB", "identifier", id, "error", err)
				}
			}
		}
		if class, ok := body["classification"]; ok {
			allowed := map[string]bool{"FRIENDLY": true, "HOSTILE": true, "NEUTRAL": true, "SUSPECT": true, "UNKNOWN": true}
			if allowed[class] {
				s.state.SetDroneClassification(id, class)
				if s.store != nil {
					if err := s.store.UpdateDroneClassification(id, class); err != nil {
						slog.Warn("Failed to persist classification to DB", "identifier", id, "error", err)
					}
				}
			}
		}

		drone, _ := s.state.GetDrone(id)
		s.writeJSON(w, drone)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleTaps returns all taps
func (s *Server) handleTaps(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	taps := s.state.GetAllTaps()
	s.writeJSON(w, taps)
}

// handleStats returns summary statistics
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	stats := s.state.GetStats()
	s.writeJSON(w, stats)
}

// handleWebSocket handles WebSocket connections
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Authenticate WebSocket connection if auth is enabled
	if s.authInt != nil {
		authenticated := false

		// Preferred: one-time ticket (never exposes JWT in URL/logs)
		if ticket := r.URL.Query().Get("ticket"); ticket != "" {
			if t := s.consumeWSTicket(ticket); t != nil {
				authenticated = true
			}
		}

		// Fallback: JWT from cookie (same-origin connections)
		if !authenticated {
			if cookie, err := r.Cookie("skylens_token"); err == nil && cookie.Value != "" {
				if _, _, err := s.authInt.Service().ValidateToken(r.Context(), cookie.Value); err == nil {
					authenticated = true
				}
			}
		}

		if !authenticated {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}

	slog.Info("WebSocket client connected", "remote", r.RemoteAddr)

	client := &wsClient{conn: conn}
	s.wsMu.Lock()
	s.wsClients[conn] = client
	s.wsMu.Unlock()

	// Set up pong handler for connection health
	conn.SetPongHandler(func(appData string) error {
		conn.SetReadDeadline(time.Now().Add(wsPingInterval + wsPongTimeout))
		return nil
	})

	// Send initial state
	s.sendInitialState(client)

	// Start ping goroutine for connection health
	go s.wsPingLoop(client)

	// Read loop (for client commands)
	go s.wsReadLoop(conn)
}

// wsPingLoop sends periodic pings to keep connection alive
func (s *Server) wsPingLoop(client *wsClient) {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	for {
		<-ticker.C

		// Check if connection is still registered
		s.wsMu.RLock()
		_, exists := s.wsClients[client.conn]
		s.wsMu.RUnlock()

		if !exists {
			return
		}

		client.writeMu.Lock()
		client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		err := client.conn.WriteMessage(websocket.PingMessage, nil)
		client.writeMu.Unlock()

		if err != nil {
			slog.Debug("WebSocket ping failed", "error", err)
			return
		}
	}
}

// sendInitialState sends current state to new WebSocket client as a single batched message
func (s *Server) sendInitialState(client *wsClient) {
	drones := s.state.GetAllDrones()
	taps := s.state.GetAllTaps()

	events := make([]map[string]interface{}, 0, len(drones)+len(taps))
	for _, d := range drones {
		events = append(events, map[string]interface{}{
			"type": "drone_update",
			"data": d,
		})
	}
	for _, t := range taps {
		events = append(events, map[string]interface{}{
			"type": "tap_status",
			"data": t,
		})
	}

	if len(events) == 0 {
		return
	}

	// Send as a single batch message (1 write instead of N)
	batch := map[string]interface{}{
		"type":   "batch",
		"events": events,
	}
	s.wsWrite(client, batch)
}

// wsReadLoop handles incoming WebSocket messages
func (s *Server) wsReadLoop(conn *websocket.Conn) {
	defer func() {
		s.wsMu.Lock()
		delete(s.wsClients, conn)
		s.wsMu.Unlock()
		conn.Close()
		slog.Info("WebSocket client disconnected")
	}()

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Warn("WebSocket read error", "error", err)
			}
			return
		}
		// Handle client commands here if needed
	}
}

// wsWrite sends a message to a WebSocket client (thread-safe).
// Closes the connection on write failure so the read loop removes it.
func (s *Server) wsWrite(client *wsClient, msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	client.writeMu.Lock()
	client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := client.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		client.conn.Close() // Triggers cleanup in read loop
	}
	client.writeMu.Unlock()
}

// collectEvents adds state events to the batch for efficient broadcasting
func (s *Server) collectEvents(eventCh <-chan processor.StateEvent) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("collectEvents panic recovered", "panic", r)
		}
	}()
	for event := range eventCh {
		msg := map[string]interface{}{
			"type": event.Type,
			"data": event.Data,
		}

		// drone_new and drone_lost are rare, high-priority events — send immediately
		// to shave up to 16ms off the user seeing "new contact" or "contact lost".
		// drone_update (high frequency) still goes through the batch + dedup path.
		if event.Type == "drone_new" || event.Type == "drone_lost" {
			s.sendImmediate(msg)
		} else {
			// Add to batch
			s.wsBatcher.mu.Lock()
			s.wsBatcher.events = append(s.wsBatcher.events, msg)
			shouldFlush := len(s.wsBatcher.events) >= wsBatchMaxSize
			s.wsBatcher.mu.Unlock()

			// Trigger immediate flush if batch is full
			if shouldFlush {
				select {
				case s.wsBatcher.flushCh <- struct{}{}:
				default:
					// Flush already pending
				}
			}
		}

		// Generate alerts based on event type
		s.generateEventAlerts(event)

		// Also broadcast to SSE clients immediately (SSE has its own buffering)
		sseEventType := event.Type
		switch event.Type {
		case "drone_new":
			sseEventType = "uav_new"
		case "drone_update":
			sseEventType = "uav_update"
		case "drone_lost":
			sseEventType = "uav_lost"
		}
		broadcastSSE(sseEventType, event.Data)
	}
}

// sendImmediate writes a single event to all WS clients without batching.
func (s *Server) sendImmediate(msg map[string]interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("Failed to marshal immediate event", "error", err)
		return
	}

	s.wsMu.RLock()
	clients := make([]*wsClient, 0, len(s.wsClients))
	for _, c := range s.wsClients {
		clients = append(clients, c)
	}
	s.wsMu.RUnlock()

	for _, client := range clients {
		client.writeMu.Lock()
		client.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := client.conn.WriteMessage(websocket.TextMessage, data); err != nil {
			client.conn.Close() // Triggers cleanup in read loop
		}
		client.writeMu.Unlock()
	}
}

// generateEventAlerts creates alerts based on state events
func (s *Server) generateEventAlerts(event processor.StateEvent) {
	switch event.Type {
	case "drone_new":
		if drone, ok := event.Data.(*processor.Drone); ok {
			if drone.IsController {
				return
			}
			alertKey := "new:" + drone.Identifier
			s.alertedMu.Lock()
			if _, alerted := s.alerted[alertKey]; !alerted {
				s.alerted[alertKey] = struct{}{}
				s.alertedMu.Unlock()

				identifier := drone.Identifier
				if drone.Designation != "" {
					identifier = drone.Designation
				}
				AddAlertWithLocation("medium", "new_drone", drone.Identifier,
					fmt.Sprintf("New drone detected: %s", identifier),
					drone.Latitude, drone.Longitude)
			} else {
				s.alertedMu.Unlock()
			}
		}

	case "drone_lost":
		if drone, ok := event.Data.(*processor.Drone); ok {
			if drone.IsController {
				return
			}
			alertKey := "lost:" + drone.Identifier
			s.alertedMu.Lock()
			if _, alerted := s.alerted[alertKey]; !alerted {
				s.alerted[alertKey] = struct{}{}
				s.alertedMu.Unlock()

				identifier := drone.Identifier
				if drone.Designation != "" {
					identifier = drone.Designation
				}
				AddAlertWithLocation("low", "drone_lost", drone.Identifier,
					fmt.Sprintf("Lost contact with drone: %s", identifier),
					drone.Latitude, drone.Longitude)
			} else {
				s.alertedMu.Unlock()
			}
		}

	case "drone_update":
		// Check for spoof detection (low trust + spoof flags)
		if drone, ok := event.Data.(*processor.Drone); ok {
			if drone.IsController {
				return
			}
			if drone.TrustScore < 50 && len(drone.SpoofFlags) > 0 {
				// Only alert once per drone (check if we've already alerted)
				alertKey := "spoof:" + drone.Identifier
				s.alertedMu.Lock()
				if _, alerted := s.alerted[alertKey]; !alerted {
					s.alerted[alertKey] = struct{}{}
					s.alertedMu.Unlock()

					identifier := drone.Identifier
					if drone.Designation != "" {
						identifier = drone.Designation
					}
					AddAlertWithLocation("critical", "spoof_detected", drone.Identifier,
						fmt.Sprintf("Possible spoofing detected: %s (trust: %d%%, flags: %v)",
							identifier, drone.TrustScore, drone.SpoofFlags),
						drone.Latitude, drone.Longitude)
				} else {
					s.alertedMu.Unlock()
				}
			}
		}

	case "tap_status":
		if tap, ok := event.Data.(*processor.Tap); ok {
			tapKey := "tap:" + tap.ID
			s.alertedMu.Lock()
			lastStatus, known := s.tapStatus[tapKey]

			if known && lastStatus != tap.Status {
				// Status actually changed
				s.tapStatus[tapKey] = tap.Status
				s.alertedMu.Unlock()

				if tap.Status == "offline" {
					AddAlert("high", "tap_offline", tap.ID,
						fmt.Sprintf("TAP offline: %s", tap.Name))
				} else if tap.Status == "online" {
					AddAlert("low", "tap_online", tap.ID,
						fmt.Sprintf("TAP back online: %s", tap.Name))
				}
			} else if !known {
				// First time seeing this TAP - just record status, don't alert
				s.tapStatus[tapKey] = tap.Status
				s.alertedMu.Unlock()
			} else {
				s.alertedMu.Unlock()
			}
		}
	}
}

// flushBatchedEvents periodically sends batched events to WebSocket clients
func (s *Server) flushBatchedEvents() {
	ticker := time.NewTicker(wsBatchInterval)
	defer ticker.Stop()

	// Periodic cleanup of the alerted map to prevent unbounded growth
	cleanupTicker := time.NewTicker(10 * time.Minute)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ticker.C:
			s.flushWSBatch()
		case <-s.wsBatcher.flushCh:
			s.flushWSBatch()
		case <-cleanupTicker.C:
			s.alertedMu.Lock()
			// Reset alerted map - allows re-alerting on persistent threats
			s.alerted = make(map[string]struct{})
			s.alertedMu.Unlock()
		}
	}
}

// Shutdown signals background goroutines (WS batcher, alert cleanup) to stop.
func (s *Server) Shutdown() {
	close(s.shutdownCh)
}

// flushWSBatch sends all pending events to WebSocket clients as a batch
func (s *Server) flushWSBatch() {
	// Grab the batch
	s.wsBatcher.mu.Lock()
	if len(s.wsBatcher.events) == 0 {
		s.wsBatcher.mu.Unlock()
		return
	}

	// Swap out the events slice
	events := s.wsBatcher.events
	s.wsBatcher.events = make([]map[string]interface{}, 0, wsBatchMaxSize)
	s.wsBatcher.mu.Unlock()

	// Dedup drone_update events — keep only the latest per identifier.
	// When 3 TAPs detect the same drone within 50ms, only the last state matters.
	seen := make(map[string]int, len(events))
	deduped := make([]map[string]interface{}, 0, len(events))
	for _, ev := range events {
		evType, _ := ev["type"].(string)
		if evType == "drone_update" {
			if drone, ok := ev["data"].(*processor.Drone); ok && drone.Identifier != "" {
				if idx, exists := seen[drone.Identifier]; exists {
					deduped[idx] = ev // replace with newer
					continue
				}
				seen[drone.Identifier] = len(deduped)
			}
		}
		deduped = append(deduped, ev)
	}
	events = deduped

	// Create batch message
	var data []byte
	var err error

	if len(events) == 1 {
		// Single event - send as-is for compatibility
		data, err = json.Marshal(events[0])
	} else {
		// Multiple events - wrap in batch array
		batch := map[string]interface{}{
			"type":   "batch",
			"events": events,
		}
		data, err = json.Marshal(batch)
	}

	if err != nil {
		slog.Error("Failed to marshal batch", "error", err, "count", len(events))
		return
	}

	// Snapshot client list under lock, then write outside it.
	// This prevents a slow client from blocking new WS connections.
	s.wsMu.RLock()
	clients := make([]*wsClient, 0, len(s.wsClients))
	for _, c := range s.wsClients {
		clients = append(clients, c)
	}
	s.wsMu.RUnlock()

	// Write to each client outside the global lock
	for _, client := range clients {
		client.writeMu.Lock()
		client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := client.conn.WriteMessage(websocket.TextMessage, data); err != nil {
			client.conn.Close() // Ensure cleanup even if read loop already exited
		}
		client.writeMu.Unlock()
	}

	if len(clients) > 0 && len(events) > 1 {
		slog.Debug("Sent batched events", "events", len(events), "clients", len(clients))
	}
}

// writeJSON sends JSON response
func (s *Server) writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// setCORSHeaders adds CORS headers with origin validation against AllowedOrigins config.
// Only origins explicitly listed in AllowedOrigins (or "*") are reflected back.
// This prevents malicious websites from making credentialed cross-origin requests.
func (s *Server) setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin != "" {
		allowed := false
		for _, o := range s.cfg.AllowedOrigins {
			if o == "*" || o == origin {
				allowed = true
				break
			}
		}
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		w.Header().Set("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, X-CSRF-Token")
}

// serveStaticFile serves a static file from the embedded filesystem
func (s *Server) serveStaticFile(w http.ResponseWriter, r *http.Request, filename string) {
	staticFS, err := fs.Sub(staticFiles, "dashboard/static")
	if err != nil {
		// Fallback to local file
		http.ServeFile(w, r, "internal/api/dashboard/static/"+filename)
		return
	}

	f, err := staticFS.Open(filename)
	if err != nil {
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, filename, stat.ModTime(), f.(io.ReadSeeker))
}

// handleTestDrone creates a test drone for UI verification
func (s *Server) handleTestDrone(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	preset := r.URL.Query().Get("preset")

	// TAP locations for reference:
	// Tap 1: 18.253187, -65.644547
	// Tap 2: 18.252459, -65.640278
	// Center point: ~18.2528, -65.6424

	// Test drone presets - positioned near actual TAPs with operator locations
	testDrones := map[string]*processor.Drone{
		"dji-mini": {
			Identifier:         "TEST-DJI-001",
			Designation:        "DJI Mini 3 Pro",
			Manufacturer:       "DJI",
			Model:              "Mini 3 Pro",
			SerialNumber:       "1ZNBH2K00C0001",
			MACAddress:         "60:60:1F:AA:BB:CC",
			Latitude:           18.2535,           // Near Tap 1
			Longitude:          -65.6440,
			AltitudeGeodetic:   45.5,
			Speed:              8.2,
			Heading:            90.0,
			GroundTrack:        90.0,
			RSSI:               -56,
			DetectionSource:    "WiFi",
			TrustScore:         95,
			Classification:     "FRIENDLY",
			TapID:              "tap-001",
			OperatorLatitude:   18.2528,          // Operator on ground nearby
			OperatorLongitude:  -65.6448,
			OperatorAltitude:   5.0,
			OperatorID:         "OP-DJI-001",
		},
		"suspicious": {
			Identifier:         "TEST-SUS-001",
			Designation:        "Unknown Drone",
			Manufacturer:       "Unknown",
			MACAddress:         "00:11:22:33:44:55",
			Latitude:           18.2540,           // Between taps
			Longitude:          -65.6420,
			AltitudeGeodetic:   120.0,
			Speed:              25.5,
			Heading:            180.0,
			GroundTrack:        180.0,
			RSSI:               -72,
			DetectionSource:    "WiFi",
			TrustScore:         35,
			Classification:     "HOSTILE",
			SpoofFlags:         []string{"no_serial", "coordinate_jump"},
			TapID:              "tap-002",
			// No operator - suspicious!
		},
		"parrot": {
			Identifier:         "TEST-PAR-001",
			Designation:        "Parrot Anafi",
			Manufacturer:       "Parrot",
			Model:              "Anafi",
			SerialNumber:       "PI040461AB0001",
			MACAddress:         "90:03:B7:DD:EE:FF",
			Latitude:           18.2522,           // Near Tap 2
			Longitude:          -65.6405,
			AltitudeGeodetic:   30.0,
			Speed:              5.0,
			Heading:            270.0,
			GroundTrack:        270.0,
			RSSI:               -48,
			DetectionSource:    "WiFi",
			TrustScore:         88,
			Classification:     "NEUTRAL",
			TapID:              "tap-002",
			OperatorLatitude:   18.2518,
			OperatorLongitude:  -65.6400,
			OperatorAltitude:   3.0,
			OperatorID:         "OP-PAR-001",
		},
		"wifi-only": {
			Identifier:         "TEST-WIFI-001",
			Designation:        "WiFi Detection",
			Manufacturer:       "DJI",
			Model:              "Unknown Model",
			MACAddress:         "60:60:1F:11:22:33",
			Latitude:           0,  // NO GPS - only RSSI
			Longitude:          0,
			RSSI:               -68,
			Channel:            149,
			DetectionSource:    "WiFiFingerprint",
			TrustScore:         60,
			Classification:     "UNVERIFIED",
			TapID:              "tap-001",
		},
	}

	drone, ok := testDrones[preset]
	if !ok {
		drone = testDrones["dji-mini"]
	}

	// Update state
	_, _ = s.state.UpdateDrone(drone)

	s.writeJSON(w, map[string]interface{}{
		"ok":    true,
		"drone": drone,
	})
}

// handleTestTap creates a test tap for UI verification
func (s *Server) handleTestTap(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	tap := &processor.Tap{
		ID:               "tap-test",
		Name:             "Test Sensor",
		Latitude:         18.4671,
		Longitude:        -66.1185,
		Altitude:         10.0,
		Status:           "online",
		Version:          "0.1.0",
		FramesTotal:      12500,
		PacketsCaptured:  12500,
		PacketsFiltered:  450,
		DetectionsSent:   125,
		CurrentChannel:   6,
		PacketsPerSecond: 85.5,
		CPUPercent:       23.5,
		MemoryPercent:    45.2,
		Temperature:      42.0,
		CaptureRunning:   true,
		TapUptime:        3600,
	}

	s.state.UpdateTap(tap)

	s.writeJSON(w, map[string]interface{}{
		"ok":  true,
		"tap": tap,
	})
}

var (
	simulationRunning bool
	simulationCancel  context.CancelFunc
	simulationMu      sync.Mutex
)

// handleSimulate starts/stops drone simulation
func (s *Server) handleSimulate(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	action := r.URL.Query().Get("action")

	simulationMu.Lock()
	defer simulationMu.Unlock()

	if action == "stop" {
		if simulationCancel != nil {
			simulationCancel()
			simulationRunning = false
		}
		s.writeJSON(w, map[string]interface{}{"ok": true, "status": "stopped"})
		return
	}

	if simulationRunning {
		s.writeJSON(w, map[string]interface{}{"ok": true, "status": "already_running"})
		return
	}

	// Start simulation
	ctx, cancel := context.WithCancel(context.Background())
	simulationCancel = cancel
	simulationRunning = true

	go s.runSimulation(ctx)

	s.writeJSON(w, map[string]interface{}{"ok": true, "status": "started"})
}

func (s *Server) runSimulation(ctx context.Context) {
	// TAP locations for reference:
	// Tap 1: 18.253187, -65.644547
	// Tap 2: 18.252459, -65.640278
	// Area bounds: lat 18.250 to 18.256, lng -65.648 to -65.638

	type simDrone struct {
		drone    *processor.Drone
		velLat   float64
		velLng   float64
		velAlt   float32
		turnRate float32
	}

	drones := []*simDrone{
		{
			drone: &processor.Drone{
				Identifier:        "SIM-DJI-001",
				Designation:       "DJI Mavic 3",
				Manufacturer:      "DJI",
				Model:             "Mavic 3",
				SerialNumber:      "1ZNBH3K00D0001",
				MACAddress:        "60:60:1F:11:22:33",
				Latitude:          18.2535,
				Longitude:         -65.6440,
				AltitudeGeodetic:  50.0,
				Speed:             12.0,
				Heading:           45.0,
				GroundTrack:       45.0,
				RSSI:              -52,
				DetectionSource:   "WiFi",
				TrustScore:        98,
				Classification:    "FRIENDLY",
				TapID:             "tap-001",
				OperatorLatitude:  18.2530,
				OperatorLongitude: -65.6445,
				OperatorAltitude:  5.0,
				OperatorID:        "OP-MAVIC-001",
			},
			velLat: 0.00004, velLng: 0.00005, velAlt: 0.3, turnRate: 1.5,
		},
		{
			drone: &processor.Drone{
				Identifier:        "SIM-SKY-002",
				Designation:       "Skydio 2+",
				Manufacturer:      "Skydio",
				Model:             "2+",
				SerialNumber:      "SDK2P00A0001",
				MACAddress:        "B8:27:EB:44:55:66",
				Latitude:          18.2520,
				Longitude:         -65.6410,
				AltitudeGeodetic:  35.0,
				Speed:             8.5,
				Heading:           180.0,
				GroundTrack:       180.0,
				RSSI:              -61,
				DetectionSource:   "WiFi",
				TrustScore:        92,
				Classification:    "FRIENDLY",
				TapID:             "tap-002",
				OperatorLatitude:  18.2515,
				OperatorLongitude: -65.6405,
				OperatorAltitude:  3.0,
				OperatorID:        "OP-SKYDIO-002",
			},
			velLat: -0.00003, velLng: 0.00004, velAlt: -0.2, turnRate: -1.0,
		},
		{
			drone: &processor.Drone{
				Identifier:       "SIM-UNK-003",
				Designation:      "Unknown UAV",
				Manufacturer:     "Unknown",
				MACAddress:       "AA:BB:CC:DD:EE:FF",
				Latitude:         18.2545,
				Longitude:        -65.6420,
				AltitudeGeodetic: 95.0,
				Speed:            22.0,
				Heading:          270.0,
				GroundTrack:      270.0,
				RSSI:             -78,
				DetectionSource:  "WiFi",
				TrustScore:       28,
				Classification:   "HOSTILE",
				SpoofFlags:       []string{"no_serial", "speed_violation"},
				TapID:            "tap-001",
				// No operator - suspicious drone!
			},
			velLat: 0.00006, velLng: -0.00008, velAlt: 0.8, turnRate: 3.0,
		},
		{
			drone: &processor.Drone{
				Identifier:        "SIM-AUT-004",
				Designation:       "Autel EVO II",
				Manufacturer:      "Autel",
				Model:             "EVO II Pro",
				SerialNumber:      "AU7N2D00B0042",
				MACAddress:        "74:4D:28:AA:BB:CC",
				Latitude:          18.2528,
				Longitude:         -65.6425,
				AltitudeGeodetic:  65.0,
				Speed:             15.0,
				Heading:           135.0,
				GroundTrack:       135.0,
				RSSI:              -55,
				DetectionSource:   "WiFi",
				TrustScore:        85,
				Classification:    "NEUTRAL",
				TapID:             "tap-002",
				OperatorLatitude:  18.2525,
				OperatorLongitude: -65.6430,
				OperatorAltitude:  4.0,
				OperatorID:        "OP-AUTEL-004",
			},
			velLat: -0.00005, velLng: -0.00003, velAlt: 0.4, turnRate: 2.0,
		},
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	detCount := uint64(0)
	saveCounter := 0

	for {
		select {
		case <-ctx.Done():
			slog.Info("Simulation stopped")
			return
		case <-ticker.C:
			saveCounter++
			for _, sd := range drones {
				d := sd.drone

				// Move drone
				d.Latitude += sd.velLat
				d.Longitude += sd.velLng
				d.AltitudeGeodetic += sd.velAlt
				d.Heading += sd.turnRate
				d.GroundTrack = d.Heading

				// Wrap heading
				if d.Heading >= 360 {
					d.Heading -= 360
				} else if d.Heading < 0 {
					d.Heading += 360
				}
				d.GroundTrack = d.Heading

				// Bounce altitude (10-150m)
				if d.AltitudeGeodetic > 150 || d.AltitudeGeodetic < 10 {
					sd.velAlt = -sd.velAlt
				}

				// Bounce position (stay near TAPs)
				if d.Latitude > 18.256 || d.Latitude < 18.250 {
					sd.velLat = -sd.velLat
				}
				if d.Longitude > -65.638 || d.Longitude < -65.648 {
					sd.velLng = -sd.velLng
				}

				// RSSI variation
				d.RSSI += int32((time.Now().UnixNano()%3) - 1)
				if d.RSSI > -40 {
					d.RSSI = -40
				}
				if d.RSSI < -90 {
					d.RSSI = -90
				}

				// Update speed based on movement
				d.Speed = float32(math.Sqrt(sd.velLat*sd.velLat+sd.velLng*sd.velLng) * 111000)

				_, _ = s.state.UpdateDrone(d)
				detCount++

				// Save to database every 2 seconds (4 ticks) for flight history
				if saveCounter%4 == 0 {
					if s.store != nil {
						if err := s.store.SaveDetection(d, false); err != nil {
							slog.Error("Simulation save failed", "id", d.Identifier, "error", err)
						}
					} else {
						slog.Warn("Store is nil in simulation")
					}
				}
			}
		}
	}
}

// handleThreat returns current threat assessment
func (s *Server) handleThreat(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	drones := s.state.GetAllDrones()

	// Count threats
	spoofingSuspects := 0
	unknownDrones := 0
	hostileCount := 0

	for _, d := range drones {
		if d.TrustScore < 50 {
			spoofingSuspects++
		}
		if d.Classification == "UNKNOWN" || d.Classification == "" {
			unknownDrones++
		}
		if d.Classification == "HOSTILE" {
			hostileCount++
		}
	}

	// Count recent critical alerts (last 5 minutes)
	alertMu.Lock()
	recentCritical := 0
	fiveMinAgo := time.Now().Add(-5 * time.Minute)
	for _, a := range alerts {
		if a.Priority == "CRITICAL" && a.Timestamp.After(fiveMinAgo) {
			recentCritical++
		}
	}
	alertMu.Unlock()

	// Determine threat level
	level := "LOW"
	if spoofingSuspects > 0 || unknownDrones > 0 {
		level = "MODERATE"
	}
	if spoofingSuspects > 2 || hostileCount > 0 || recentCritical > 0 {
		level = "HIGH"
	}
	if hostileCount > 2 || recentCritical > 2 {
		level = "CRITICAL"
	}

	s.writeJSON(w, map[string]interface{}{
		"threat_level":           level,
		"spoofing_suspects":      spoofingSuspects,
		"unknown_drones":         unknownDrones,
		"recent_critical_alerts": recentCritical,
		"hostile_count":          hostileCount,
	})
}

// handleFleet returns all UAVs for fleet view
func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	drones := s.state.GetAllDrones()
	s.writeJSON(w, map[string]interface{}{
		"uavs":  drones,
		"total": len(drones),
	})
}

// Alert represents a system alert
type Alert struct {
	ID         string    `json:"id"`
	Priority   string    `json:"priority"`
	Type       string    `json:"alert_type"`
	Identifier string    `json:"uav_identifier"`
	Message    string    `json:"message"`
	Timestamp  time.Time `json:"timestamp"`
	Acked      bool      `json:"acknowledged"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	Latitude   *float64  `json:"latitude,omitempty"`
	Longitude  *float64  `json:"longitude,omitempty"`
	MGRS       string    `json:"mgrs,omitempty"`
}

// Alert configuration
const (
	maxAlerts     = 1000          // Maximum alerts to keep in memory
	alertTTL      = 24 * time.Hour // Alerts expire after 24 hours
	alertCleanup  = 5 * time.Minute // Cleanup interval
)

var alerts = make([]Alert, 0)
var alertMu sync.Mutex
var alertCleanupOnce sync.Once
var alertStore *storage.Store        // set by Server.Start for DB persistence
var alertNotifier func(Alert)        // optional callback for external notifications (e.g. Telegram)
var telegramTestFn func() error      // send a test message via Telegram (set from main)
var telegramInstance *notify.Telegram // runtime-reconfigurable Telegram notifier
var takPublisher *tak.Publisher       // TAK publisher for runtime reconfiguration (set from main)
var takClient *tak.Client             // TAK client for connection management (set from main)

// =============================================================================
// Suspect Management
// =============================================================================

// Suspect represents a potential UAV candidate pending confirmation
type Suspect struct {
	ID              string    `json:"id"`
	MACAddress      string    `json:"mac_address"`
	MACPrefix       string    `json:"mac_prefix"`        // First 3 octets (OUI)
	SSID            string    `json:"ssid,omitempty"`
	Channel         int32     `json:"channel,omitempty"`
	ChannelBand     string    `json:"channel_band,omitempty"` // "2.4GHz" or "5GHz"
	BeaconInterval  uint32    `json:"beacon_interval_tu,omitempty"`
	RSSI            int32     `json:"rssi,omitempty"`
	DetectionCount  int       `json:"detection_count"`
	Manufacturer    string    `json:"manufacturer,omitempty"`   // Suspected/guessed
	Model           string    `json:"model,omitempty"`          // Suspected/guessed
	Confidence      float32   `json:"confidence"`               // Detection confidence
	Status          string    `json:"status"`                   // "pending", "confirmed", "dismissed"
	FirstSeen       time.Time `json:"first_seen"`
	LastSeen        time.Time `json:"last_seen"`
	TapID           string    `json:"tap_id,omitempty"`
	SuspectReasons  []string  `json:"suspect_reasons,omitempty"` // Why this is suspected as UAV
}

var suspects = make(map[string]*Suspect)
var suspectsMu sync.RWMutex

// AddSuspect adds or updates a suspect in the tracking list
func AddSuspect(s *Suspect) {
	suspectsMu.Lock()
	defer suspectsMu.Unlock()

	existing, exists := suspects[s.MACAddress]
	if !exists {
		s.FirstSeen = time.Now()
		s.LastSeen = time.Now()
		s.DetectionCount = 1
		s.Status = "pending"
		suspects[s.MACAddress] = s

		// Broadcast SSE event
		broadcastSSE("suspect_new", s)
		return
	}

	// Update existing suspect
	existing.LastSeen = time.Now()
	existing.DetectionCount++
	if s.RSSI != 0 {
		existing.RSSI = s.RSSI
	}
	if s.Channel != 0 {
		existing.Channel = s.Channel
	}
	if s.SSID != "" {
		existing.SSID = s.SSID
	}
	if s.TapID != "" {
		existing.TapID = s.TapID
	}
	// Increase confidence with repeated detections
	if existing.Confidence < 0.95 {
		existing.Confidence += 0.02
	}
}

// GetSuspect returns a suspect by MAC address
func GetSuspect(mac string) (*Suspect, bool) {
	suspectsMu.RLock()
	defer suspectsMu.RUnlock()
	s, ok := suspects[mac]
	return s, ok
}

// GetAllSuspects returns all suspects
func GetAllSuspects() []*Suspect {
	suspectsMu.RLock()
	defer suspectsMu.RUnlock()

	result := make([]*Suspect, 0, len(suspects))
	for _, s := range suspects {
		result = append(result, s)
	}
	return result
}

// GetPendingSuspects returns only pending suspects
func GetPendingSuspects() []*Suspect {
	suspectsMu.RLock()
	defer suspectsMu.RUnlock()

	result := make([]*Suspect, 0)
	for _, s := range suspects {
		if s.Status == "pending" {
			result = append(result, s)
		}
	}
	return result
}

// ConfirmSuspect marks a suspect as confirmed and creates a learned signature
func ConfirmSuspect(mac, manufacturer, model string) *Suspect {
	suspectsMu.Lock()
	defer suspectsMu.Unlock()

	s, ok := suspects[mac]
	if !ok {
		return nil
	}

	s.Status = "confirmed"
	s.Manufacturer = manufacturer
	s.Model = model
	s.Confidence = 1.0

	// Broadcast promotion event
	broadcastSSE("suspect_promoted", s)
	return s
}

// DismissSuspect marks a suspect as dismissed (false positive)
func DismissSuspect(mac, reason string) *Suspect {
	suspectsMu.Lock()
	defer suspectsMu.Unlock()

	s, ok := suspects[mac]
	if !ok {
		return nil
	}

	s.Status = "dismissed"
	if reason != "" {
		s.SuspectReasons = append(s.SuspectReasons, "dismissed: "+reason)
	}

	// Broadcast dismissal event
	broadcastSSE("suspect_dismissed", s)
	return s
}

// RemoveSuspect removes a suspect from tracking
func RemoveSuspect(mac string) bool {
	suspectsMu.Lock()
	defer suspectsMu.Unlock()

	_, ok := suspects[mac]
	if ok {
		delete(suspects, mac)
	}
	return ok
}

// startSuspectCleanup starts the background suspect cleanup goroutine
var suspectCleanupOnce sync.Once

func startSuspectCleanup(done <-chan struct{}) {
	suspectCleanupOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					cleanupSuspects()
				}
			}
		}()
	})
}

// cleanupSuspects removes old suspects (not seen in 2 minutes)
func cleanupSuspects() {
	suspectsMu.Lock()
	defer suspectsMu.Unlock()

	now := time.Now()
	maxAge := 2 * time.Minute

	for mac, s := range suspects {
		// Only cleanup pending suspects - confirmed/dismissed stay for reference
		if s.Status == "pending" && now.Sub(s.LastSeen) > maxAge {
			delete(suspects, mac)
		}
	}
}

// ExtractMACPrefix extracts the OUI (first 3 octets) from a MAC address
func ExtractMACPrefix(mac string) string {
	// MAC format: XX:XX:XX:XX:XX:XX or XX-XX-XX-XX-XX-XX
	mac = strings.ReplaceAll(mac, "-", ":")
	parts := strings.Split(mac, ":")
	if len(parts) >= 3 {
		return strings.ToUpper(parts[0] + ":" + parts[1] + ":" + parts[2])
	}
	return strings.ToUpper(mac)
}

// GetChannelBand determines the frequency band from channel number
func GetChannelBand(channel int32) string {
	if channel >= 1 && channel <= 14 {
		return "2.4GHz"
	} else if channel >= 36 && channel <= 177 {
		return "5GHz"
	}
	return "unknown"
}

// startAlertCleanup starts the background alert cleanup goroutine
func startAlertCleanup(done <-chan struct{}) {
	alertCleanupOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(alertCleanup)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					cleanupAlerts()
				}
			}
		}()
	})
}

// cleanupAlerts removes expired and excess alerts
func cleanupAlerts() {
	alertMu.Lock()
	now := time.Now()

	// Remove expired alerts
	filtered := make([]Alert, 0, len(alerts))
	for _, a := range alerts {
		if !a.ExpiresAt.IsZero() && now.After(a.ExpiresAt) {
			continue // Skip expired
		}
		filtered = append(filtered, a)
	}

	// Trim to max size (keep newest)
	if len(filtered) > maxAlerts {
		filtered = filtered[len(filtered)-maxAlerts:]
	}

	alerts = filtered
	alertMu.Unlock()

	// Also cleanup expired alerts in the database
	if st := alertStore; st != nil {
		if n, err := st.CleanupExpiredAlerts(); err != nil {
			slog.Warn("Failed to cleanup expired alerts in DB", "error", err)
		} else if n > 0 {
			slog.Debug("Cleaned up expired alerts from DB", "removed", n)
		}
	}
}

// SetAlertNotifier registers an external notification callback (e.g. Telegram)
func SetAlertNotifier(fn func(Alert)) {
	alertNotifier = fn
}

// SetTelegramTestFn registers a function to send a test Telegram message
func SetTelegramTestFn(fn func() error) {
	telegramTestFn = fn
}

// SetTelegramInstance stores the Telegram notifier for runtime reconfiguration
func SetTelegramInstance(tg *notify.Telegram) {
	telegramInstance = tg
}

// SetTAKPublisher stores the TAK publisher for runtime reconfiguration
func SetTAKPublisher(pub *tak.Publisher) {
	takPublisher = pub
}

// SetTAKClient stores the TAK client for connection management
func SetTAKClient(c *tak.Client) {
	takClient = c
}

// normalizePriority converts legacy lowercase priorities to uppercase labels expected by the frontend.
func normalizePriority(p string) string {
	switch strings.ToLower(p) {
	case "critical":
		return "CRITICAL"
	case "high":
		return "HIGH"
	case "medium":
		return "MEDIUM"
	case "low":
		return "INFO"
	default:
		return strings.ToUpper(p)
	}
}

// AddAlert adds a new alert with automatic TTL
func AddAlert(priority, alertType, identifier, message string) {
	priority = normalizePriority(priority)

	now := time.Now()
	alert := Alert{
		ID:         fmt.Sprintf("alert-%d", now.UnixNano()),
		Priority:   priority,
		Type:       alertType,
		Identifier: identifier,
		Message:    message,
		Timestamp:  now,
		Acked:      false,
		ExpiresAt:  now.Add(alertTTL),
	}

	alertMu.Lock()
	alerts = append(alerts, alert)
	if len(alerts) > maxAlerts {
		alerts = alerts[1:]
	}
	alertMu.Unlock()

	// Persist to database (non-blocking)
	if st := alertStore; st != nil {
		go func() {
			if err := st.InsertAlert(storage.AlertRow{
				ID:        alert.ID,
				Priority:  alert.Priority,
				AlertType: alert.Type,
				Identifier: alert.Identifier,
				Message:   alert.Message,
				Acked:     false,
				CreatedAt: alert.Timestamp,
				ExpiresAt: alert.ExpiresAt,
			}); err != nil {
				slog.Warn("Failed to persist alert", "id", alert.ID, "error", err)
			}
		}()
	}

	// Broadcast to SSE clients
	broadcastSSE("alert", alert)

	// External notification (Telegram etc.)
	if fn := alertNotifier; fn != nil {
		go fn(alert)
	}
}

// AddAlertWithLocation adds an alert with optional lat/lon + MGRS grid reference.
// Location is only attached when not the 0,0 sentinel.
func AddAlertWithLocation(priority, alertType, identifier, message string, lat, lon float64) {
	AddAlert(priority, alertType, identifier, message)

	// Attach location to the just-added alert if coordinates are real
	if lat == 0 && lon == 0 {
		return
	}
	alertMu.Lock()
	if len(alerts) > 0 {
		a := &alerts[len(alerts)-1]
		a.Latitude = &lat
		a.Longitude = &lon
		a.MGRS = geo.LatLonToMGRS(lat, lon, 4)
	}
	alertMu.Unlock()
}

// handleAlerts returns all alerts
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	alertMu.Lock()
	defer alertMu.Unlock()

	s.writeJSON(w, alerts)
}

// handleAlertByID handles /api/alert/{id}/ack
func (s *Server) handleAlertByID(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	// Handle /api/alert/{id}/ack
	if r.Method == "POST" {
		path := r.URL.Path
		// Extract ID from /api/alert/{id}/ack
		path = strings.TrimPrefix(path, "/api/alert/")
		id := strings.TrimSuffix(path, "/ack")

		alertMu.Lock()
		for i := range alerts {
			if alerts[i].ID == id {
				alerts[i].Acked = true
				break
			}
		}
		alertMu.Unlock()

		if s.store != nil {
			go s.store.AckAlert(id)
		}
		s.writeJSON(w, map[string]bool{"ok": true})
	}
}

// handleAlertsAction handles /api/alerts/ack-all and /api/alerts/clear
func (s *Server) handleAlertsAction(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch r.URL.Path {
	case "/api/alerts/ack-all":
		alertMu.Lock()
		for i := range alerts {
			alerts[i].Acked = true
		}
		alertMu.Unlock()
		if s.store != nil {
			go s.store.AckAllAlerts()
		}
		s.writeJSON(w, map[string]bool{"ok": true})

	case "/api/alerts/clear":
		alertMu.Lock()
		alerts = make([]Alert, 0)
		alertMu.Unlock()
		if s.store != nil {
			go s.store.ClearAlerts()
		}
		s.writeJSON(w, map[string]bool{"ok": true})

	default:
		http.NotFound(w, r)
	}
}

// handleTelegramTest sends a test message via the server-side Telegram integration
func (s *Server) handleTelegramTest(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	fn := telegramTestFn
	if fn == nil {
		s.writeJSON(w, map[string]interface{}{"ok": false, "error": "Telegram notifier not initialized"})
		return
	}

	if err := fn(); err != nil {
		s.writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	s.writeJSON(w, map[string]bool{"ok": true})
}

// handleTelegramStatus returns Telegram config and whether it's active
// GET: returns current config (token masked)
// POST: saves new config to DB and updates runtime
func (s *Server) handleTelegramStatus(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	tg := telegramInstance

	if r.Method == "GET" {
		if tg == nil {
			s.writeJSON(w, map[string]interface{}{
				"enabled": false,
				"bot_token": "",
				"chat_id": "",
				"notify_new_drone": true,
				"notify_spoofing": true,
				"notify_drone_lost": true,
				"notify_tap_status": true,
			})
			return
		}
		cfg := tg.GetConfig()
		// Mask the bot token (show last 4 chars only)
		maskedToken := ""
		if cfg.BotToken != "" {
			if len(cfg.BotToken) > 4 {
				maskedToken = strings.Repeat("*", len(cfg.BotToken)-4) + cfg.BotToken[len(cfg.BotToken)-4:]
			} else {
				maskedToken = "****"
			}
		}
		s.writeJSON(w, map[string]interface{}{
			"enabled":           cfg.IsConfigured(),
			"bot_token":         maskedToken,
			"chat_id":           cfg.ChatID,
			"notify_new_drone":  cfg.NotifyNewDrone,
			"notify_spoofing":   cfg.NotifySpoofing,
			"notify_drone_lost": cfg.NotifyDroneLost,
			"notify_tap_status": cfg.NotifyTapStatus,
		})
		return
	}

	if r.Method == "POST" {
		var req struct {
			BotToken        string `json:"bot_token"`
			ChatID          string `json:"chat_id"`
			NotifyNewDrone  *bool  `json:"notify_new_drone"`
			NotifySpoofing  *bool  `json:"notify_spoofing"`
			NotifyDroneLost *bool  `json:"notify_drone_lost"`
			NotifyTapStatus *bool  `json:"notify_tap_status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		// Build new config: start from current, overlay provided fields
		var newCfg notify.TelegramConfig
		if tg != nil {
			newCfg = tg.GetConfig()
		} else {
			newCfg = notify.TelegramConfig{
				NotifyNewDrone: true, NotifySpoofing: true,
				NotifyDroneLost: true, NotifyTapStatus: true,
			}
		}

		// Only update bot_token if non-empty and not masked
		if req.BotToken != "" && !strings.HasPrefix(req.BotToken, "***") {
			newCfg.BotToken = req.BotToken
		}
		if req.ChatID != "" {
			newCfg.ChatID = req.ChatID
		}
		if req.NotifyNewDrone != nil {
			newCfg.NotifyNewDrone = *req.NotifyNewDrone
		}
		if req.NotifySpoofing != nil {
			newCfg.NotifySpoofing = *req.NotifySpoofing
		}
		if req.NotifyDroneLost != nil {
			newCfg.NotifyDroneLost = *req.NotifyDroneLost
		}
		if req.NotifyTapStatus != nil {
			newCfg.NotifyTapStatus = *req.NotifyTapStatus
		}

		// Save to database
		if s.store != nil {
			boolStr := func(b bool) string { if b { return "true" }; return "false" }
			settings := map[string]string{
				"tg_bot_token":         newCfg.BotToken,
				"tg_chat_id":           newCfg.ChatID,
				"tg_notify_new_drone":  boolStr(newCfg.NotifyNewDrone),
				"tg_notify_spoofing":   boolStr(newCfg.NotifySpoofing),
				"tg_notify_drone_lost": boolStr(newCfg.NotifyDroneLost),
				"tg_notify_tap_status": boolStr(newCfg.NotifyTapStatus),
			}
			for k, v := range settings {
				if err := s.store.SetSetting(k, v); err != nil {
					slog.Warn("Failed to save telegram setting", "key", k, "error", err)
				}
			}
		}

		// Update runtime config
		if tg != nil {
			tg.UpdateConfig(newCfg)
		}

		s.writeJSON(w, map[string]interface{}{"ok": true, "enabled": newCfg.IsConfigured()})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// handleTAKStatus returns TAK config and connection state (GET) or saves config (POST)
func (s *Server) handleTAKStatus(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	if r.Method == "GET" {
		var resp = map[string]interface{}{
			"enabled":          false,
			"connected":        false,
			"address":          "",
			"use_tls":          true,
			"cert_file":        "",
			"key_file":         "",
			"ca_file":          "",
			"rate_limit_sec":   3,
			"stale_seconds":    30,
			"send_controllers": false,
			"last_error":       "",
		}
		if takPublisher != nil {
			cfg := takPublisher.GetConfig()
			resp["enabled"] = cfg.Enabled
			resp["rate_limit_sec"] = cfg.RateLimitSec
			resp["stale_seconds"] = cfg.StaleSeconds
			resp["send_controllers"] = cfg.SendControllers
		}
		if takClient != nil {
			ccfg := takClient.GetConfig()
			resp["connected"] = takClient.IsConnected()
			resp["address"] = ccfg.Address
			resp["use_tls"] = ccfg.UseTLS
			resp["cert_file"] = ccfg.CertFile
			resp["key_file"] = ccfg.KeyFile
			resp["ca_file"] = ccfg.CAFile
			resp["last_error"] = takClient.LastError()
		}
		s.writeJSON(w, resp)
		return
	}

	if r.Method == "POST" {
		var req struct {
			Enabled         *bool   `json:"enabled"`
			Address         string  `json:"address"`
			UseTLS          *bool   `json:"use_tls"`
			RateLimitSec    *int    `json:"rate_limit_sec"`
			StaleSeconds    *int    `json:"stale_seconds"`
			SendControllers *bool   `json:"send_controllers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		// Build new configs from current
		var pubCfg tak.PublisherConfig
		var clientCfg tak.ClientConfig
		if takPublisher != nil {
			pubCfg = takPublisher.GetConfig()
		}
		if takClient != nil {
			clientCfg = takClient.GetConfig()
		}

		if req.Enabled != nil {
			pubCfg.Enabled = *req.Enabled
		}
		if req.Address != "" {
			clientCfg.Address = req.Address
		}
		if req.UseTLS != nil {
			clientCfg.UseTLS = *req.UseTLS
		}
		if req.RateLimitSec != nil {
			pubCfg.RateLimitSec = *req.RateLimitSec
		}
		if req.StaleSeconds != nil {
			pubCfg.StaleSeconds = *req.StaleSeconds
		}
		if req.SendControllers != nil {
			pubCfg.SendControllers = *req.SendControllers
		}

		// Persist to database
		if s.store != nil {
			boolStr := func(b bool) string {
				if b {
					return "true"
				}
				return "false"
			}
			settings := map[string]string{
				"tak_enabled":          boolStr(pubCfg.Enabled),
				"tak_address":          clientCfg.Address,
				"tak_use_tls":          boolStr(clientCfg.UseTLS),
				"tak_cert_file":        clientCfg.CertFile,
				"tak_key_file":         clientCfg.KeyFile,
				"tak_ca_file":          clientCfg.CAFile,
				"tak_rate_limit_sec":   fmt.Sprintf("%d", pubCfg.RateLimitSec),
				"tak_stale_seconds":    fmt.Sprintf("%d", pubCfg.StaleSeconds),
				"tak_send_controllers": boolStr(pubCfg.SendControllers),
			}
			for k, v := range settings {
				if err := s.store.SetSetting(k, v); err != nil {
					slog.Warn("Failed to save TAK setting", "key", k, "error", err)
				}
			}
		}

		// Update runtime
		if takPublisher != nil {
			takPublisher.UpdateConfig(pubCfg)
		}
		if takClient != nil {
			takClient.UpdateConfig(clientCfg)
		}

		s.writeJSON(w, map[string]interface{}{"ok": true})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// handleTAKTest tests the TAK server connection
func (s *Server) handleTAKTest(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if takClient == nil {
		s.writeJSON(w, map[string]interface{}{"ok": false, "error": "TAK client not initialized"})
		return
	}

	cfg := takClient.GetConfig()
	if cfg.Address == "" {
		s.writeJSON(w, map[string]interface{}{"ok": false, "error": "No TAK server address configured"})
		return
	}

	if err := tak.TestConnection(cfg); err != nil {
		s.writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	s.writeJSON(w, map[string]interface{}{"ok": true, "message": "Connected to " + cfg.Address})
}

// takCertsDir resolves the certificate storage directory.
// Prefers /etc/skylens/certs/ (production), falls back to ./certs/ (dev).
func takCertsDir() string {
	primary := "/etc/skylens/certs"
	if err := os.MkdirAll(primary, 0750); err == nil {
		return primary
	}
	// Fallback for non-root / dev environments
	fallback := "certs"
	if err := os.MkdirAll(fallback, 0750); err != nil {
		slog.Warn("Failed to create certs directory", "primary", primary, "fallback", fallback, "error", err)
	}
	return fallback
}

// handleTAKUploadCert handles certificate file uploads for TAK TLS
func (s *Server) handleTAKUploadCert(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit upload to 100KB — certs are small text files
	r.Body = http.MaxBytesReader(w, r.Body, 100<<10)

	if err := r.ParseMultipartForm(100 << 10); err != nil {
		s.writeJSON(w, map[string]interface{}{"ok": false, "error": "Upload failed (max 100KB): " + err.Error()})
		return
	}

	certType := r.FormValue("type") // "cert", "key", "ca"
	if certType != "cert" && certType != "key" && certType != "ca" {
		s.writeJSON(w, map[string]interface{}{"ok": false, "error": "Invalid cert type (must be cert, key, or ca)"})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		s.writeJSON(w, map[string]interface{}{"ok": false, "error": "No file in upload"})
		return
	}
	defer file.Close()

	// Read file content
	data, err := io.ReadAll(file)
	if err != nil {
		s.writeJSON(w, map[string]interface{}{"ok": false, "error": "Failed to read uploaded file"})
		return
	}

	if len(data) == 0 {
		s.writeJSON(w, map[string]interface{}{"ok": false, "error": "Uploaded file is empty"})
		return
	}

	// Validate PEM format — all TAK certs must be PEM-encoded
	if !isPEM(data) {
		s.writeJSON(w, map[string]interface{}{"ok": false, "error": "File is not PEM-encoded. Export your TAK certs as .pem files (BEGIN CERTIFICATE / BEGIN PRIVATE KEY / BEGIN RSA PRIVATE KEY)."})
		return
	}

	// Resolve cert directory
	certsDir := takCertsDir()

	// Determine filename
	var filename string
	switch certType {
	case "cert":
		filename = "tak-client-cert.pem"
	case "key":
		filename = "tak-client-key.pem"
	case "ca":
		filename = "tak-ca.pem"
	}

	destPath := certsDir + "/" + filename

	// Atomic write: write to temp file, then rename
	perm := os.FileMode(0640)
	if certType == "key" {
		perm = 0600
	}

	tmpPath := destPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, perm); err != nil {
		s.writeJSON(w, map[string]interface{}{"ok": false, "error": "Failed to write file: " + err.Error()})
		return
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		// Rename failed (cross-device?), fall back to direct write
		os.Remove(tmpPath)
		if err := os.WriteFile(destPath, data, perm); err != nil {
			s.writeJSON(w, map[string]interface{}{"ok": false, "error": "Failed to save file: " + err.Error()})
			return
		}
	}

	// Update TAK client config and persist to DB
	if takClient != nil {
		cfg := takClient.GetConfig()
		switch certType {
		case "cert":
			cfg.CertFile = destPath
		case "key":
			cfg.KeyFile = destPath
		case "ca":
			cfg.CAFile = destPath
		}
		takClient.UpdateConfig(cfg)

		// Persist path to DB
		if s.store != nil {
			var settingKey string
			switch certType {
			case "cert":
				settingKey = "tak_cert_file"
			case "key":
				settingKey = "tak_key_file"
			case "ca":
				settingKey = "tak_ca_file"
			}
			s.store.SetSetting(settingKey, destPath)
		}
	}

	slog.Info("TAK certificate uploaded", "type", certType, "file", header.Filename, "size", len(data), "path", destPath)
	s.writeJSON(w, map[string]interface{}{
		"ok":       true,
		"filename": header.Filename,
		"path":     destPath,
	})
}

// isPEM checks if data looks like a PEM-encoded file.
func isPEM(data []byte) bool {
	s := string(data)
	return strings.Contains(s, "-----BEGIN ")
}

// SSE clients
var sseClients = make(map[chan []byte]bool)
var sseMu sync.RWMutex

// handleSSE handles Server-Sent Events for real-time updates
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering

	// Create client channel
	clientChan := make(chan []byte, 100)

	sseMu.Lock()
	sseClients[clientChan] = true
	sseMu.Unlock()

	// Remove client on disconnect
	defer func() {
		sseMu.Lock()
		delete(sseClients, clientChan)
		sseMu.Unlock()
		close(clientChan)
	}()

	// Get flusher
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	// Send initial connected event
	fmt.Fprintf(w, "event: connected\ndata: {}\n\n")
	flusher.Flush()

	// Keepalive ticker - send ping every 15 seconds to keep connection alive
	// This prevents browsers from closing the connection when minimized
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	// Listen for events
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			// Send SSE comment as keepalive ping
			// Comments start with : and are ignored by SSE parsers
			_, err := w.Write([]byte(": keepalive\n\n"))
			if err != nil {
				// Connection is dead, exit
				return
			}
			flusher.Flush()
		case msg, ok := <-clientChan:
			if !ok {
				return
			}
			_, err := w.Write(msg)
			if err != nil {
				// Connection is dead, exit
				return
			}
			flusher.Flush()
		}
	}
}

// broadcastSSE sends an event to all SSE clients
func broadcastSSE(eventType string, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}

	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, string(jsonData))
	msgBytes := []byte(msg)

	sseMu.RLock()
	clientCount := len(sseClients)
	sseMu.RUnlock()

	if clientCount == 0 {
		return
	}

	sseMu.RLock()
	for clientChan := range sseClients {
		// Non-blocking send with timeout fallback
		select {
		case clientChan <- msgBytes:
			// Sent successfully
		default:
			// Channel is full - client is slow/dead, skip this message
			// The client will be cleaned up by keepalive failure
		}
	}
	sseMu.RUnlock()
}

// handleSystemStats returns system statistics
func (s *Server) handleSystemStats(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	stats := s.state.GetStats()

	// Get real CPU usage (average over 200ms)
	cpuPercent := 0.0
	if cpuPcts, err := cpu.Percent(200*time.Millisecond, false); err == nil && len(cpuPcts) > 0 {
		cpuPercent = cpuPcts[0]
	}

	// Get real memory usage
	memPercent := 0.0
	if memInfo, err := mem.VirtualMemory(); err == nil {
		memPercent = memInfo.UsedPercent
	}

	// Get real disk usage
	diskPercent := 0.0
	if diskInfo, err := disk.Usage("/"); err == nil {
		diskPercent = diskInfo.UsedPercent
	}

	// Determine DB status
	dbStatus := "disconnected"
	if s.store != nil {
		dbStatus = "connected"
	}

	// Get pipeline health stats from receiver
	var pipeline map[string]interface{}
	if s.receiver != nil {
		pipeline = s.receiver.GetPipelineStats()
	}

	s.writeJSON(w, map[string]interface{}{
		"cpu_percent":    math.Round(cpuPercent*10) / 10,
		"memory_percent": math.Round(memPercent*10) / 10,
		"disk_percent":   math.Round(diskPercent*10) / 10,
		"uptime":         time.Since(s.startTime).Seconds(),
		"version":        "0.1.0",
		"node_name":      "skylens-node",
		"node_uuid":      "skylens-001",
		"messages":       stats["drones_total"],
		"uavs_tracked":   stats["drones_total"],
		"taps_online":    stats["taps_online"],
		"db_status":      dbStatus,
		"pipeline":       pipeline,
	})
}

// handleClearData clears all test data
func (s *Server) handleIntelUpdate(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Resolve paths from binary working directory
	jsonPath := "internal/intel/drone_models.json"
	tapJsonPath := "tap/intel/drone_models.json"
	goPath := "internal/intel/fingerprint.go"

	// Check if TAP path exists
	if _, err := os.Stat(tapJsonPath); os.IsNotExist(err) {
		tapJsonPath = ""
	}

	result, err := intel.RunIntelUpdate(jsonPath, tapJsonPath, goPath, false)
	if err != nil {
		slog.Error("Intel update failed", "error", err)
		s.writeJSON(w, map[string]interface{}{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	details := make([]string, 0, len(result.NewOUIs))
	for oui, label := range result.NewOUIs {
		details = append(details, oui+" → "+label)
	}

	s.writeJSON(w, map[string]interface{}{
		"ok":       true,
		"version":  result.NewVersion,
		"new_ouis": len(result.NewOUIs),
		"source":   result.Source,
		"details":  details,
	})
}

func (s *Server) handleClearData(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	// Stop simulation if running
	if simulationCancel != nil {
		simulationCancel()
		simulationRunning = false
	}

	// Clear alerts
	alertMu.Lock()
	alerts = make([]Alert, 0)
	alertMu.Unlock()

	s.writeJSON(w, map[string]bool{"ok": true})
}

// handleUAVAction handles /api/uav/{id}/hide, /api/uav/{id}/delete, /api/uav/{id}/history
func (s *Server) handleUAVAction(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	// Parse path: /api/uav/{id}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/uav/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	id := parts[0]
	action := parts[1]

	switch action {
	case "hide":
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.state.SetDroneHidden(id, true)
		s.writeJSON(w, map[string]bool{"ok": true})

	case "delete":
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if drone, ok := s.state.GetDrone(id); ok {
			slog.Warn("Drone deleted via dashboard",
				"identifier", id,
				"track_number", drone.TrackNumber,
				"manufacturer", drone.Manufacturer,
				"model", drone.Model,
				"designation", drone.Designation,
				"remote_addr", r.RemoteAddr,
			)
		}
		s.state.DeleteDrone(id)
		s.writeJSON(w, map[string]bool{"ok": true})

	case "history":
		// Return flight history for a drone from database detections
		positions, stats, err := s.store.GetFlightHistory(id, 1000)

		// Get operator position from drone state or database
		var operator map[string]interface{}
		drone, hasDrone := s.state.GetDrone(id)
		if hasDrone && drone.OperatorLatitude != 0 && drone.OperatorLongitude != 0 {
			operator = map[string]interface{}{
				"lat": drone.OperatorLatitude,
				"lng": drone.OperatorLongitude,
				"alt": drone.OperatorAltitude,
			}
		}

		if err != nil {
			slog.Warn("Failed to get flight history", "identifier", id, "error", err)
			// Fall back to current position from state
			if !hasDrone {
				http.Error(w, "Drone not found", http.StatusNotFound)
				return
			}
			response := map[string]interface{}{
				"identifier": id,
				"positions": []map[string]interface{}{
					{
						"time":     drone.LastSeen,
						"lat":      drone.Latitude,
						"lng":      drone.Longitude,
						"altitude": drone.AltitudeGeodetic,
						"speed":    drone.Speed,
						"heading":  drone.Heading,
						"op_lat":   drone.OperatorLatitude,
						"op_lng":   drone.OperatorLongitude,
					},
				},
				"stats": map[string]interface{}{
					"position_count":    1,
					"total_distance_m":  0,
					"max_altitude_m":    drone.AltitudeGeodetic,
					"max_speed_ms":      drone.Speed,
					"duration_s":        0,
				},
			}
			if operator != nil {
				response["operator"] = operator
			}
			s.writeJSON(w, response)
			return
		}

		if len(positions) == 0 {
			// No history in DB, check current state
			if !hasDrone {
				http.Error(w, "Drone not found", http.StatusNotFound)
				return
			}
			// Return current position as single-point history
			response := map[string]interface{}{
				"identifier": id,
				"positions": []map[string]interface{}{
					{
						"time":     drone.LastSeen,
						"lat":      drone.Latitude,
						"lng":      drone.Longitude,
						"altitude": drone.AltitudeGeodetic,
						"speed":    drone.Speed,
						"heading":  drone.Heading,
						"op_lat":   drone.OperatorLatitude,
						"op_lng":   drone.OperatorLongitude,
					},
				},
				"stats": map[string]interface{}{
					"position_count":    1,
					"total_distance_m":  0,
					"max_altitude_m":    drone.AltitudeGeodetic,
					"max_speed_ms":      drone.Speed,
					"duration_s":        0,
				},
			}
			if operator != nil {
				response["operator"] = operator
			}
			s.writeJSON(w, response)
			return
		}

		response := map[string]interface{}{
			"identifier": id,
			"positions":  positions,
			"stats":      stats,
		}
		if operator != nil {
			response["operator"] = operator
		}
		s.writeJSON(w, response)

	case "tag":
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Tag string `json:"tag"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		s.state.SetDroneTag(id, body.Tag)
		if s.store != nil {
			if err := s.store.UpdateDroneTag(id, body.Tag); err != nil {
				slog.Warn("Failed to persist tag to DB", "identifier", id, "error", err)
			}
		}
		s.writeJSON(w, map[string]bool{"ok": true})

	case "classify":
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Classification string `json:"classification"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		allowed := map[string]bool{"FRIENDLY": true, "HOSTILE": true, "NEUTRAL": true, "SUSPECT": true, "UNKNOWN": true}
		if !allowed[body.Classification] {
			http.Error(w, "Invalid classification. Allowed: FRIENDLY, HOSTILE, NEUTRAL, SUSPECT, UNKNOWN", http.StatusBadRequest)
			return
		}
		s.state.SetDroneClassification(id, body.Classification)
		if s.store != nil {
			if err := s.store.UpdateDroneClassification(id, body.Classification); err != nil {
				slog.Warn("Failed to persist classification to DB", "identifier", id, "error", err)
			}
		}
		s.writeJSON(w, map[string]bool{"ok": true})

	case "telegram":
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		drone, ok := s.state.GetDrone(id)
		if !ok {
			http.Error(w, "Drone not found", http.StatusNotFound)
			return
		}
		if telegramInstance == nil {
			s.writeJSON(w, map[string]interface{}{"ok": false, "error": "Telegram not configured"})
			return
		}
		report := notify.DroneReport{
			TrackNumber:       drone.TrackNumber,
			Designation:       drone.Designation,
			DetectionSource:   drone.DetectionSource,
			SerialNumber:      drone.SerialNumber,
			Lat:               drone.Latitude,
			Lon:               drone.Longitude,
			AltitudeM:         drone.AltitudeGeodetic,
			SpeedMS:           drone.Speed,
			VerticalSpeedMS:   drone.VerticalSpeed,
			Heading:           drone.Heading,
			RSSI:              drone.RSSI,
			OperationalStatus: drone.OperationalStatus,
			FirstSeen:         drone.FirstSeen,
			LastSeen:          drone.LastSeen,
			DetectionCount:    drone.DetectionCount,
			OperatorLat:       drone.OperatorLatitude,
			OperatorLon:       drone.OperatorLongitude,
			ReportTime:        time.Now(),
		}
		if err := telegramInstance.SendDroneReport(report); err != nil {
			slog.Warn("Manual Telegram report failed", "identifier", id, "error", err)
			s.writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		s.writeJSON(w, map[string]bool{"ok": true})

	case "sightings":
		if r.Method != "GET" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s.store == nil {
			s.writeJSON(w, map[string]interface{}{"sightings": []struct{}{}, "total": 0})
			return
		}
		sightings, err := s.store.GetDroneSightings(id)
		if err != nil {
			slog.Warn("Failed to get sightings", "identifier", id, "error", err)
			http.Error(w, "Failed to get sightings", http.StatusInternalServerError)
			return
		}
		if sightings == nil {
			sightings = []storage.Sighting{}
		}
		s.writeJSON(w, map[string]interface{}{"sightings": sightings, "total": len(sightings)})

	default:
		http.NotFound(w, r)
	}
}

// handleHideLost hides all lost drones
func (s *Server) handleHideLost(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.state.HideAllLost()
	s.writeJSON(w, map[string]bool{"ok": true})
}

// handleUnhideAll unhides all drones
func (s *Server) handleUnhideAll(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.state.UnhideAll()
	s.writeJSON(w, map[string]bool{"ok": true})
}

// handleTapCommand handles TAP command endpoints
// POST /api/tap/{id}/ping - Send ping command
// POST /api/tap/{id}/restart - Send restart command
// GET  /api/tap/{id}/config - Get current TAP config
// POST /api/tap/{id}/config - Update TAP config
func (s *Server) handleTapCommand(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	// Parse path first: /api/tap/{id}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/tap/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		http.Error(w, "Invalid path - expected /api/tap/{id}/{action}", http.StatusBadRequest)
		return
	}

	tapID := parts[0]
	action := parts[1]

	// Allow GET for config, POST for everything else
	if action == "config" {
		if r.Method != "GET" && r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
	} else if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.receiver == nil {
		http.Error(w, "NATS receiver not configured", http.StatusServiceUnavailable)
		return
	}

	// Verify tap exists and get tap data
	taps := s.state.GetAllTaps()
	tapExists := false
	var tapData *processor.Tap
	for _, t := range taps {
		if t.ID == tapID {
			tapExists = true
			tapData = t
			break
		}
	}
	if !tapExists && tapID != "test" {
		http.Error(w, "TAP not found", http.StatusNotFound)
		return
	}

	switch action {
	case "ping":
		commandID, err := s.receiver.SendPingCommand(tapID)
		if err != nil {
			s.writeJSON(w, map[string]interface{}{
				"ok":    false,
				"error": err.Error(),
			})
			return
		}

		// Wait for ack with timeout
		ack, err := s.receiver.WaitForAck(commandID, 5*time.Second)
		if err != nil {
			s.writeJSON(w, map[string]interface{}{
				"ok":         false,
				"command_id": commandID,
				"error":      err.Error(),
			})
			return
		}

		s.writeJSON(w, map[string]interface{}{
			"ok":         true,
			"command_id": commandID,
			"tap_id":     ack.TapID,
			"latency_ms": float64(ack.LatencyNs) / 1e6,
			"success":    ack.Success,
		})

	case "restart":
		var body struct {
			Graceful bool `json:"graceful"`
		}
		body.Graceful = true // Default to graceful
		json.NewDecoder(r.Body).Decode(&body)

		commandID, err := s.receiver.SendRestartCommand(tapID, body.Graceful)
		if err != nil {
			s.writeJSON(w, map[string]interface{}{
				"ok":    false,
				"error": err.Error(),
			})
			return
		}

		s.writeJSON(w, map[string]interface{}{
			"ok":         true,
			"command_id": commandID,
			"tap_id":     tapID,
			"graceful":   body.Graceful,
			"message":    "Restart command sent",
		})

	case "config":
		if r.Method == "GET" {
			// Return current known config from heartbeat data
			var currentChannel int32
			var tapName string
			var seenChannels []int32
			var bleScanning bool
			if tapData != nil {
				currentChannel = tapData.CurrentChannel
				tapName = tapData.Name
				seenChannels = tapData.SeenChannels
				bleScanning = tapData.BLEScanning
			}
			if seenChannels == nil {
				seenChannels = []int32{}
			}
			s.writeJSON(w, map[string]interface{}{
				"ok":     true,
				"tap_id": tapID,
				"config": map[string]interface{}{
					"current_channel": currentChannel,
					"channels":        seenChannels,
					"hop_interval_ms": 200,
					"ble_enabled":     bleScanning,
					"log_level":       "info",
					"tap_name":        tapName,
				},
			})
			return
		}

		// POST — apply config changes
		var body struct {
			Channels      []int32 `json:"channels"`
			HopIntervalMs int32   `json:"hop_interval_ms"`
			BLEEnabled    *bool   `json:"ble_enabled"`
			LogLevel      string  `json:"log_level"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.writeJSON(w, map[string]interface{}{
				"ok":    false,
				"error": "Invalid request body: " + err.Error(),
			})
			return
		}

		var commandIDs []string

		// Send SetChannelsCommand if channels provided
		if len(body.Channels) > 0 {
			hopInterval := body.HopIntervalMs
			if hopInterval <= 0 {
				hopInterval = 200
			}
			cmdID, err := s.receiver.SendSetChannelsCommand(tapID, body.Channels, hopInterval)
			if err != nil {
				s.writeJSON(w, map[string]interface{}{
					"ok":    false,
					"error": "Failed to send channel config: " + err.Error(),
				})
				return
			}
			commandIDs = append(commandIDs, cmdID)
		}

		// Send UpdateConfigCommand for key-value config pairs
		configMap := make(map[string]string)
		if body.BLEEnabled != nil {
			if *body.BLEEnabled {
				configMap["ble_enabled"] = "true"
			} else {
				configMap["ble_enabled"] = "false"
			}
		}
		if body.LogLevel != "" {
			configMap["log_level"] = body.LogLevel
		}

		if len(configMap) > 0 {
			cmdID, err := s.receiver.SendUpdateConfigCommand(tapID, configMap)
			if err != nil {
				s.writeJSON(w, map[string]interface{}{
					"ok":    false,
					"error": "Failed to send config update: " + err.Error(),
				})
				return
			}
			commandIDs = append(commandIDs, cmdID)
		}

		if len(commandIDs) == 0 {
			s.writeJSON(w, map[string]interface{}{
				"ok":      true,
				"tap_id":  tapID,
				"message": "No config changes to apply",
			})
			return
		}

		tapDisplayName := tapID
		if tapData != nil && tapData.Name != "" {
			tapDisplayName = tapData.Name
		}
		s.writeJSON(w, map[string]interface{}{
			"ok":          true,
			"tap_id":      tapID,
			"command_ids": commandIDs,
			"message":     "Config sent to " + tapDisplayName,
		})

	default:
		http.Error(w, "Unknown action: "+action, http.StatusBadRequest)
	}
}

// handleBroadcast handles broadcast commands to all TAPs
// POST /api/taps/broadcast/ping - Ping all TAPs
// POST /api/taps/broadcast/restart - Restart all TAPs
func (s *Server) handleBroadcast(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.receiver == nil {
		http.Error(w, "NATS receiver not configured", http.StatusServiceUnavailable)
		return
	}

	// Parse command from path
	path := strings.TrimPrefix(r.URL.Path, "/api/taps/broadcast/")
	command := strings.TrimSuffix(path, "/")

	switch command {
	case "ping":
		commandID, err := s.receiver.SendPingCommand("*")
		if err != nil {
			s.writeJSON(w, map[string]interface{}{
				"ok":    false,
				"error": err.Error(),
			})
			return
		}

		s.writeJSON(w, map[string]interface{}{
			"ok":         true,
			"command_id": commandID,
			"broadcast":  true,
			"message":    "Ping broadcast sent to all TAPs",
		})

	case "restart":
		var body struct {
			Graceful bool `json:"graceful"`
		}
		body.Graceful = true
		json.NewDecoder(r.Body).Decode(&body)

		commandID, err := s.receiver.SendRestartCommand("*", body.Graceful)
		if err != nil {
			s.writeJSON(w, map[string]interface{}{
				"ok":    false,
				"error": err.Error(),
			})
			return
		}

		s.writeJSON(w, map[string]interface{}{
			"ok":         true,
			"command_id": commandID,
			"broadcast":  true,
			"graceful":   body.Graceful,
			"message":    "Restart broadcast sent to all TAPs",
		})

	default:
		http.Error(w, "Unknown broadcast command: "+command, http.StatusBadRequest)
	}
}

// handleDetectionsHistory returns historical detection data for analytics
// GET /api/detections/history?hours=24&limit=1000
func (s *Server) handleDetectionsHistory(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	// Parse query params
	hoursStr := r.URL.Query().Get("hours")
	hours := 24 // default 24 hours
	if hoursStr != "" {
		if h, err := fmt.Sscanf(hoursStr, "%d", &hours); err == nil && h > 0 {
			if hours > 720 { // Max 30 days
				hours = 720
			}
		}
	}

	limitStr := r.URL.Query().Get("limit")
	limit := 1000 // default
	if limitStr != "" {
		if l, err := fmt.Sscanf(limitStr, "%d", &limit); err == nil && l > 0 {
			if limit > 10000 {
				limit = 10000
			}
		}
	}

	// Get historical detections from store
	detections := []map[string]interface{}{}

	if s.store != nil {
		// Try to get from database
		rows, err := s.store.GetRecentDetections(hours, limit)
		if err != nil {
			slog.Warn("Failed to get detections history", "error", err)
		} else {
			detections = rows
		}
	}

	// Supplement with drone first_seen timestamps to show when drones were discovered
	// This ensures analytics reflect actual discovery dates even without full detection history
	drones := s.state.GetAllDrones()
	now := time.Now()
	cutoff := now.Add(-time.Duration(hours) * time.Hour)

	// Track which identifiers we already have from real detections
	seenIDs := make(map[string]bool)
	for _, det := range detections {
		if id, ok := det["identifier"].(string); ok {
			seenIDs[id] = true
		}
	}

	// Add first_seen events for drones not in real detections
	for _, d := range drones {
		if seenIDs[d.Identifier] {
			continue // already have real detection data
		}

		// Use the drone's actual first_seen timestamp
		ts := d.FirstSeen
		if ts.IsZero() {
			ts = d.LastSeen
		}
		if ts.IsZero() {
			continue
		}

		// Only include if within the requested time range
		if ts.Before(cutoff) {
			continue
		}

		detections = append(detections, map[string]interface{}{
			"identifier":     d.Identifier,
			"timestamp":      ts.Format(time.RFC3339),
			"hour":           ts.Hour(),
			"day":            int(ts.Weekday()),
			"lat":            d.Latitude,
			"lng":            d.Longitude,
			"altitude":       d.AltitudeGeodetic,
			"trust_score":    d.TrustScore,
			"classification": d.Classification,
			"manufacturer":   d.Manufacturer,
			"model":          d.Model,
			"tap_id":         d.TapID,
		})
	}

	s.writeJSON(w, map[string]interface{}{
		"detections": detections,
		"total":      len(detections),
		"hours":      hours,
		"generated":  time.Now().Format(time.RFC3339),
	})
}

// =============================================================================
// Trail API Handlers (GPS Flight Paths)
// =============================================================================

// handleTrails returns all active drone trails
// GET /api/trails - all active trails
// GET /api/trails?max_age=60 - trails active in last N seconds
func (s *Server) handleTrails(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse max_age parameter (default 60 seconds)
	maxAge := 60 * time.Second
	if ageStr := r.URL.Query().Get("max_age"); ageStr != "" {
		if secs, err := strconv.Atoi(ageStr); err == nil && secs > 0 {
			maxAge = time.Duration(secs) * time.Second
		}
	}

	// Get active trails
	trails := s.state.GetActiveTrails(maxAge)
	if trails == nil {
		trails = make(map[string][]processor.PositionSample)
	}

	// Build response with trail stats
	type TrailResponse struct {
		Identifier    string                     `json:"identifier"`
		Positions     []processor.PositionSample `json:"positions"`
		PositionCount int                        `json:"position_count"`
		Stats         *processor.TrailStats      `json:"stats,omitempty"`
	}

	result := make([]*TrailResponse, 0, len(trails))
	for id, positions := range trails {
		stats := s.state.GetTrailStats(id)
		result = append(result, &TrailResponse{
			Identifier:    id,
			Positions:     positions,
			PositionCount: len(positions),
			Stats:         stats,
		})
	}

	s.writeJSON(w, map[string]interface{}{
		"trails":      result,
		"trail_count": len(result),
		"max_age_sec": maxAge.Seconds(),
	})
}

// handleTrailByID returns trail for a specific drone
// GET /api/trails/{drone_id} - get trail for drone
// GET /api/trails/{drone_id}?since=<RFC3339> - get positions since timestamp
// GET /api/trails/{drone_id}/stats - get trail statistics only
func (s *Server) handleTrailByID(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse drone ID from path: /api/trails/{drone_id}
	path := strings.TrimPrefix(r.URL.Path, "/api/trails/")
	parts := strings.Split(path, "/")
	droneID := parts[0]

	if droneID == "" {
		http.Error(w, "Missing drone ID", http.StatusBadRequest)
		return
	}

	// Check if requesting stats only
	if len(parts) > 1 && parts[1] == "stats" {
		stats := s.state.GetTrailStats(droneID)
		if stats == nil {
			http.Error(w, "Trail not found", http.StatusNotFound)
			return
		}
		s.writeJSON(w, stats)
		return
	}

	// Get trail positions
	var positions []processor.PositionSample

	// Check for since parameter
	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		since, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			http.Error(w, "Invalid since timestamp (use RFC3339)", http.StatusBadRequest)
			return
		}
		positions = s.state.GetDroneTrailSince(droneID, since)
	} else {
		positions = s.state.GetDroneTrail(droneID)
	}

	if positions == nil {
		http.Error(w, "Trail not found", http.StatusNotFound)
		return
	}

	// Get stats too
	stats := s.state.GetTrailStats(droneID)

	s.writeJSON(w, map[string]interface{}{
		"identifier":     droneID,
		"positions":      positions,
		"position_count": len(positions),
		"stats":          stats,
	})
}

// =============================================================================
// Suspect and Signature Learning API Handlers
// =============================================================================

// handleSuspects returns all suspect candidates
// GET /api/suspects - list all suspects (from StateManager, the real tracking system)
// GET /api/suspects?status=pending - filter by status
// GET /api/suspects?source=all - include both StateManager and legacy suspects
//
// In single-TAP mode, this returns ALL suspects being tracked, not just those
// with multi-TAP confirmation. Operators can then manually confirm suspects.
func (s *Server) handleSuspects(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get suspect candidates from StateManager (the real tracking data)
	// This includes ALL suspects - single-TAP and multi-TAP alike
	candidates := s.state.GetSuspectCandidates()

	// Also get suspect stats for additional info
	suspectStats := s.state.GetSuspectStats()

	// Get correlator info for pending multi-TAP correlations
	correlator := s.state.GetCorrelator()
	pendingCorrelations := correlator.GetPendingSuspects()

	// Build response with enhanced suspect info
	type EnhancedSuspect struct {
		*processor.SuspectCandidate
		TapCount          int     `json:"tap_count"`
		TimeSpanSec       float64 `json:"time_span_sec"`
		CanAutoPromote    bool    `json:"can_auto_promote"`
		PromotionEligible string  `json:"promotion_eligible"` // "multi_tap", "mobility", "observation", "manual_only"
	}

	enhancedSuspects := make([]*EnhancedSuspect, 0, len(candidates))
	for _, c := range candidates {
		timeSpan := c.LastSeen.Sub(c.FirstSeen).Seconds()

		// Determine promotion eligibility
		eligibility := "manual_only"
		canAutoPromote := false

		if len(c.TapsSeen) >= 2 {
			eligibility = "multi_tap"
			canAutoPromote = true
		} else if c.IsLikelyMobile && c.MobilityScore >= 0.6 {
			eligibility = "mobility"
			canAutoPromote = true
		} else if c.Observations >= 5 && timeSpan >= 30 && c.MobilityScore >= 0.3 {
			eligibility = "observation"
			canAutoPromote = true
		}

		enhanced := &EnhancedSuspect{
			SuspectCandidate:  c,
			TapCount:          len(c.TapsSeen),
			TimeSpanSec:       timeSpan,
			CanAutoPromote:    canAutoPromote,
			PromotionEligible: eligibility,
		}
		enhancedSuspects = append(enhancedSuspects, enhanced)
	}

	// Include legacy suspects if requested
	source := r.URL.Query().Get("source")
	var legacySuspects []*Suspect
	if source == "all" {
		legacySuspects = GetAllSuspects()
	}

	s.writeJSON(w, map[string]interface{}{
		"suspects":             enhancedSuspects,
		"total":                len(enhancedSuspects),
		"stats":                suspectStats,
		"pending_correlations": len(pendingCorrelations),
		"legacy_suspects":      legacySuspects,
	})
}

// handleSuspectAction handles suspect confirmation and dismissal
// POST /api/suspects/{mac}/confirm - confirm suspect as drone (body: {manufacturer, model})
// POST /api/suspects/{mac}/dismiss - dismiss suspect as false positive (body: {reason})
// GET /api/suspects/{mac} - get single suspect details
//
// IMPORTANT: The confirm action uses the StateManager's ConfirmSuspect method
// which promotes the suspect to full drone tracking. This is especially useful
// in single-TAP mode where operators can manually confirm suspects.
func (s *Server) handleSuspectAction(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	// Parse path: /api/suspects/{mac}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/suspects/")
	parts := strings.Split(path, "/")
	if len(parts) < 1 {
		http.Error(w, "Invalid path - expected /api/suspects/{mac} or /api/suspects/{mac}/{action}", http.StatusBadRequest)
		return
	}

	mac := parts[0]

	// Handle GET for single suspect
	if r.Method == "GET" {
		// First check StateManager's suspect candidates (the real tracking data)
		candidate := s.state.GetSuspectCandidate(mac)
		if candidate != nil {
			s.writeJSON(w, candidate)
			return
		}
		// Fallback to legacy suspect tracking
		suspect, ok := GetSuspect(mac)
		if !ok {
			http.Error(w, "Suspect not found", http.StatusNotFound)
			return
		}
		s.writeJSON(w, suspect)
		return
	}

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if len(parts) < 2 {
		http.Error(w, "Invalid path - expected /api/suspects/{mac}/{action}", http.StatusBadRequest)
		return
	}

	action := parts[1]

	switch action {
	case "confirm":
		// Manual operator confirmation - this is a key pathway in single-TAP mode
		// Uses the StateManager's ConfirmSuspect which promotes to full drone tracking

		// First, try the StateManager's suspect candidates (the real tracking system)
		drone, err := s.state.ConfirmSuspect(mac)
		if err == nil && drone != nil {
			// Successfully promoted via StateManager
			slog.Info("Operator confirmed suspect via API",
				"mac", mac,
				"identifier", drone.Identifier,
				"trust_score", drone.TrustScore,
			)

			// Also update legacy suspect tracking if present
			ConfirmSuspect(mac, drone.Manufacturer, drone.Model)

			// Create learned signature if we have a store
			if s.store != nil {
				sig := &storage.LearnedSignature{
					MACPrefix:          ExtractMACPrefix(mac),
					Manufacturer:       drone.Manufacturer,
					Model:              drone.Model,
					ConfirmationMethod: "operator_manual",
					Confidence:         0.85,
					IsActive:           true,
				}
				if err := s.store.UpsertSignature(sig); err != nil {
					slog.Warn("Failed to save learned signature", "mac", mac, "error", err)
				}
			}

			// Queue drone for database storage
			if s.store != nil {
				s.store.SaveDetection(drone, true)
			}

			s.writeJSON(w, map[string]interface{}{
				"ok":      true,
				"status":  "confirmed",
				"source":  "operator_confirmed",
				"drone":   drone,
			})
			return
		}

		// Fallback: check legacy suspect tracking
		var body struct {
			Manufacturer string `json:"manufacturer"`
			Model        string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			// If no body and StateManager failed, return error
			if err.Error() != "EOF" {
				http.Error(w, "Invalid JSON body", http.StatusBadRequest)
				return
			}
		}

		suspect := ConfirmSuspect(mac, body.Manufacturer, body.Model)
		if suspect == nil {
			http.Error(w, "Suspect not found in either tracking system", http.StatusNotFound)
			return
		}

		// Create learned signature from confirmed suspect
		if s.store != nil {
			sig := &storage.LearnedSignature{
				MACPrefix:          ExtractMACPrefix(suspect.MACAddress),
				SSIDPattern:        suspect.SSID,
				ChannelBand:        suspect.ChannelBand,
				BeaconIntervalTU:   int(suspect.BeaconInterval),
				Manufacturer:       body.Manufacturer,
				Model:              body.Model,
				ConfirmationMethod: "manual",
				SampleCount:        suspect.DetectionCount,
				Confidence:         suspect.Confidence,
				IsActive:           true,
			}
			if err := s.store.UpsertSignature(sig); err != nil {
				slog.Warn("Failed to save learned signature", "mac", mac, "error", err)
			} else {
				slog.Info("Learned signature saved", "mac_prefix", sig.MACPrefix, "manufacturer", sig.Manufacturer, "model", sig.Model)
			}
		}

		s.writeJSON(w, map[string]interface{}{
			"ok":      true,
			"status":  "confirmed",
			"source":  "legacy",
			"suspect": suspect,
		})

	case "dismiss":
		var body struct {
			Reason string `json:"reason"`
		}
		json.NewDecoder(r.Body).Decode(&body) // Reason is optional

		// Dismiss from StateManager's tracking
		s.state.DismissSuspect(mac)

		// Also dismiss from legacy tracking
		suspect := DismissSuspect(mac, body.Reason)

		// Record false positive for tuning
		if s.store != nil {
			reasons := []string{}
			if suspect != nil {
				reasons = suspect.SuspectReasons
			}
			if err := s.store.RecordFalsePositive(mac, reasons, body.Reason); err != nil {
				slog.Warn("Failed to record false positive", "mac", mac, "error", err)
			}
		}

		s.writeJSON(w, map[string]interface{}{
			"ok":      true,
			"status":  "dismissed",
			"suspect": suspect,
		})

	default:
		http.Error(w, "Unknown action: "+action, http.StatusBadRequest)
	}
}

// handleSignatures returns all learned signatures
// GET /api/signatures - list all active signatures
// GET /api/signatures?manufacturer=DJI - filter by manufacturer
func (s *Server) handleSignatures(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.store == nil {
		// Return empty list if no database
		s.writeJSON(w, map[string]interface{}{
			"signatures": []interface{}{},
			"total":      0,
		})
		return
	}

	signatures, err := s.store.GetActiveSignatures()
	if err != nil {
		slog.Warn("Failed to get signatures", "error", err)
		s.writeJSON(w, map[string]interface{}{
			"signatures": []interface{}{},
			"total":      0,
			"error":      err.Error(),
		})
		return
	}

	// Filter by manufacturer if specified
	manufacturerFilter := r.URL.Query().Get("manufacturer")
	if manufacturerFilter != "" {
		filtered := make([]*storage.LearnedSignature, 0)
		for _, sig := range signatures {
			if strings.EqualFold(sig.Manufacturer, manufacturerFilter) {
				filtered = append(filtered, sig)
			}
		}
		signatures = filtered
	}

	s.writeJSON(w, map[string]interface{}{
		"signatures": signatures,
		"total":      len(signatures),
	})
}

// handleUserPreferences handles GET/PUT for per-account dashboard settings
func (s *Server) handleUserPreferences(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		prefs, err := s.store.GetUserPreferences(user.ID)
		if err != nil {
			slog.Warn("Failed to get user preferences", "error", err, "user_id", user.ID)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		s.writeJSON(w, prefs)

	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024))
		if err != nil {
			http.Error(w, `{"error":"read body failed"}`, http.StatusBadRequest)
			return
		}
		var prefs map[string]interface{}
		if err := json.Unmarshal(body, &prefs); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if err := s.store.SetUserPreferences(user.ID, prefs); err != nil {
			slog.Warn("Failed to set user preferences", "error", err, "user_id", user.ID)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		s.writeJSON(w, map[string]string{"status": "ok"})

	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}
