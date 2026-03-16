package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/K13094/skylens/internal/auth"
)

// AuthIntegration holds auth service and handlers
type AuthIntegration struct {
	service    *auth.Service
	middleware *auth.Middleware
	handlers   *auth.Handlers
}

// Service returns the auth service for token validation
func (a *AuthIntegration) Service() *auth.Service {
	return a.service
}

// SetupAuth initializes the authentication system
// Returns nil if auth is disabled or JWT secret is not set
func (s *Server) SetupAuth() *AuthIntegration {
	// Check if auth is enabled
	if !s.cfg.AuthEnabled {
		slog.Info("Auth disabled in config")
		return nil
	}

	// Check JWT secret
	if s.cfg.JWTSecret == "" {
		slog.Info("Auth disabled: no JWT secret configured")
		return nil
	}

	// Get database connection from store
	pgInterface := s.store.GetPG()
	redisInterface := s.store.GetRedis()

	if pgInterface == nil {
		slog.Info("Auth disabled: no database connection")
		return nil
	}

	// Type assert to proper types
	pg, ok := pgInterface.(*pgxpool.Pool)
	if !ok {
		slog.Error("Auth disabled: invalid database pool type")
		return nil
	}

	var redisClient *redis.Client
	if redisInterface != nil {
		redisClient, _ = redisInterface.(*redis.Client)
	}

	// Ensure auth schema exists
	if err := s.store.EnsureAuthSchema(); err != nil {
		slog.Error("Failed to create auth schema", "error", err)
		return nil
	}

	// Create auth config
	config := auth.DefaultConfig()
	config.JWTSecret = s.cfg.JWTSecret
	config.SecureCookies = s.cfg.SecureCookies
	if s.cfg.JWTExpiry > 0 {
		config.JWTExpiry = s.cfg.JWTExpiry
	}
	if s.cfg.RefreshExpiry > 0 {
		config.RefreshExpiry = s.cfg.RefreshExpiry
	}
	if s.cfg.SessionExpiry > 0 {
		config.SessionExpiry = s.cfg.SessionExpiry
	}

	// Create auth service
	service, err := auth.NewService(s.store.GetContext(), config, pg, redisClient)
	if err != nil {
		slog.Error("Failed to create auth service", "error", err)
		return nil
	}

	middleware := auth.NewMiddleware(service)
	handlers := auth.NewHandlers(service, middleware)

	slog.Info("Authentication system initialized")

	return &AuthIntegration{
		service:    service,
		middleware: middleware,
		handlers:   handlers,
	}
}

// RegisterAuthRoutes adds authentication routes to the server
func (s *Server) RegisterAuthRoutes(mux *http.ServeMux, authInt *AuthIntegration) {
	if authInt == nil {
		// Auth is disabled, register stub handlers
		mux.HandleFunc("/api/auth/login", s.authDisabledHandler)
		mux.HandleFunc("/api/auth/logout", s.authDisabledHandler)
		mux.HandleFunc("/api/auth/me", s.authDisabledHandler)
		return
	}

	// Public auth routes (no auth required)
	mux.HandleFunc("/api/auth/login", authInt.handlers.HandleLogin)
	mux.HandleFunc("/api/auth/refresh", authInt.handlers.HandleRefresh)
	mux.HandleFunc("/api/auth/csrf", authInt.handlers.HandleCSRFToken)
	mux.HandleFunc("/api/auth/password-requirements", authInt.handlers.HandlePasswordRequirements)

	// Protected auth routes (require authentication)
	mux.Handle("/api/auth/logout", authInt.middleware.RequireAuth(
		http.HandlerFunc(authInt.handlers.HandleLogout)))
	mux.Handle("/api/auth/me", authInt.middleware.RequireAuth(
		http.HandlerFunc(authInt.handlers.HandleMe)))
	mux.Handle("/api/auth/roles", authInt.middleware.RequireAuth(
		http.HandlerFunc(authInt.handlers.HandleListRoles)))
	mux.Handle("/api/auth/change-password", authInt.middleware.RequireAuth(
		authInt.middleware.RequireCSRF(
			http.HandlerFunc(authInt.handlers.HandleChangePassword))))
	mux.Handle("/api/auth/sessions", authInt.middleware.RequireAuth(
		http.HandlerFunc(authInt.handlers.HandleListSessions)))

	// Session revocation - protected with CSRF
	mux.Handle("/api/auth/sessions/", authInt.middleware.RequireAuth(
		authInt.middleware.RequireCSRF(
			http.HandlerFunc(authInt.handlers.HandleRevokeSession))))

	// Admin routes (require admin role)
	// Read-only admin handler (no CSRF required)
	adminReadHandler := func(h http.HandlerFunc) http.Handler {
		return authInt.middleware.RequireAuth(
			authInt.middleware.RequireRole("admin")(
				http.HandlerFunc(h)))
	}

	// Mutation admin handler (CSRF required)
	adminMutationHandler := func(h http.HandlerFunc) http.Handler {
		return authInt.middleware.RequireAuth(
			authInt.middleware.RequireRole("admin")(
				authInt.middleware.RequireCSRF(
					http.HandlerFunc(h))))
	}

	mux.Handle("/api/admin/users", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			adminReadHandler(authInt.handlers.HandleListUsers).ServeHTTP(w, r)
		case http.MethodPost:
			adminMutationHandler(authInt.handlers.HandleCreateUser).ServeHTTP(w, r)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}))

	// Individual user routes
	mux.Handle("/api/admin/users/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/admin/users/")

		// Check for reset-password action (mutation - requires CSRF)
		if strings.HasSuffix(path, "/reset-password") {
			adminMutationHandler(authInt.handlers.HandleResetPassword).ServeHTTP(w, r)
			return
		}

		// Regular user CRUD
		switch r.Method {
		case http.MethodGet:
			adminReadHandler(authInt.handlers.HandleGetUser).ServeHTTP(w, r)
		case http.MethodPut, http.MethodPatch:
			adminMutationHandler(authInt.handlers.HandleUpdateUser).ServeHTTP(w, r)
		case http.MethodDelete:
			adminMutationHandler(authInt.handlers.HandleDeleteUser).ServeHTTP(w, r)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}))
}

// authDisabledHandler returns a message when auth is disabled
func (s *Server) authDisabledHandler(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	// For /me endpoint, return a dummy user
	if strings.HasSuffix(r.URL.Path, "/me") {
		s.writeJSON(w, map[string]interface{}{
			"user": map[string]interface{}{
				"id":          0,
				"username":    "anonymous",
				"role_name":   "admin",
				"permissions": []string{"*"},
			},
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"message":"authentication is disabled"}`))
}
