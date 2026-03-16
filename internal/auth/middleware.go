package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Context keys for storing user info in request context
type contextKey string

const (
	UserContextKey     contextKey = "auth_user"
	ClaimsContextKey   contextKey = "auth_claims"
	SessionContextKey  contextKey = "auth_session"
)

// Cookie names
const (
	AccessTokenCookie  = "skylens_token"
	RefreshTokenCookie = "skylens_refresh"
	CSRFTokenCookie    = "skylens_csrf"
	CSRFHeaderName     = "X-CSRF-Token"
)

// Middleware provides HTTP middleware for authentication
type Middleware struct {
	service *Service
	config  Config
}

// NewMiddleware creates a new auth middleware
func NewMiddleware(service *Service) *Middleware {
	return &Middleware{
		service: service,
		config:  service.config,
	}
}

// RequireAuth middleware ensures the request is authenticated
func (m *Middleware) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("RequireAuth middleware called", "path", r.URL.Path)

		// Try to get token from cookie first
		token := ""
		if cookie, err := r.Cookie(AccessTokenCookie); err == nil {
			token = cookie.Value
			slog.Debug("Token found in cookie", "length", len(token))
		}

		// Fall back to Authorization header
		if token == "" {
			authHeader := r.Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				token = strings.TrimPrefix(authHeader, "Bearer ")
				slog.Debug("Token found in Authorization header", "length", len(token))
			}
		}

		if token == "" {
			slog.Debug("No token found")
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}

		// Validate token
		slog.Debug("Validating token...")
		user, claims, err := m.service.ValidateToken(r.Context(), token)
		if err != nil {
			// Try to refresh if we have a refresh token
			if refreshCookie, err := r.Cookie(RefreshTokenCookie); err == nil {
				newAccessToken, newRefreshToken, err := m.service.RefreshToken(
					r.Context(),
					refreshCookie.Value,
					getClientIP(r),
					r.UserAgent(),
				)
				if err == nil {
					// Set new cookies
					setAuthCookies(w, newAccessToken, newRefreshToken, m.config.SecureCookies, int(m.config.JWTExpiry.Seconds()))

					// Re-validate with new token
					user, claims, err = m.service.ValidateToken(r.Context(), newAccessToken)
					if err != nil {
						clearAuthCookies(w)
						http.Error(w, `{"error":"session expired"}`, http.StatusUnauthorized)
						return
					}
				} else {
					clearAuthCookies(w)
					http.Error(w, `{"error":"session expired"}`, http.StatusUnauthorized)
					return
				}
			} else {
				slog.Debug("Token validation failed, no refresh token", "error", err)
				http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
				return
			}
		}

		// Add user to context
		ctx := context.WithValue(r.Context(), UserContextKey, user)
		ctx = context.WithValue(ctx, ClaimsContextKey, claims)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireRole middleware ensures the user has a specific role
func (m *Middleware) RequireRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := GetUserFromContext(r.Context())
			if user == nil {
				http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
				return
			}

			hasRole := false
			for _, role := range roles {
				if user.HasRole(role) {
					hasRole = true
					break
				}
			}

			if !hasRole {
				m.logPermissionDenied(r, user, "role", strings.Join(roles, ","))
				http.Error(w, `{"error":"insufficient permissions"}`, http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequirePermission middleware ensures the user has a specific permission
func (m *Middleware) RequirePermission(permissions ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := GetUserFromContext(r.Context())
			if user == nil {
				http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
				return
			}

			// Admins have all permissions
			if user.IsAdmin() {
				next.ServeHTTP(w, r)
				return
			}

			hasPermission := false
			for _, perm := range permissions {
				if user.HasPermission(perm) {
					hasPermission = true
					break
				}
			}

			if !hasPermission {
				m.logPermissionDenied(r, user, "permission", strings.Join(permissions, ","))
				http.Error(w, `{"error":"insufficient permissions"}`, http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequireCSRF middleware validates CSRF tokens for state-changing requests
func (m *Middleware) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip CSRF for safe methods
		if r.Method == "GET" || r.Method == "HEAD" || r.Method == "OPTIONS" {
			next.ServeHTTP(w, r)
			return
		}

		// Skip if CSRF is disabled
		if !m.config.CSRFEnabled {
			next.ServeHTTP(w, r)
			return
		}

		// Get CSRF token from cookie
		cookie, err := r.Cookie(CSRFTokenCookie)
		if err != nil {
			http.Error(w, `{"error":"CSRF token missing"}`, http.StatusForbidden)
			return
		}

		// Get CSRF token from header
		headerToken := r.Header.Get(CSRFHeaderName)
		if headerToken == "" {
			// Also check form value
			headerToken = r.FormValue("_csrf")
		}

		if headerToken == "" {
			http.Error(w, `{"error":"CSRF token missing"}`, http.StatusForbidden)
			return
		}

		// Validate tokens match (double-submit pattern)
		if cookie.Value != headerToken {
			m.logCSRFViolation(r)
			http.Error(w, `{"error":"CSRF token invalid"}`, http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// OptionalAuth middleware extracts user info if present but doesn't require it
func (m *Middleware) OptionalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to get token from cookie
		token := ""
		if cookie, err := r.Cookie(AccessTokenCookie); err == nil {
			token = cookie.Value
		}

		// Fall back to Authorization header
		if token == "" {
			authHeader := r.Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				token = strings.TrimPrefix(authHeader, "Bearer ")
			}
		}

		if token != "" {
			// Validate token
			user, claims, err := m.service.ValidateToken(r.Context(), token)
			if err == nil {
				// Add user to context
				ctx := context.WithValue(r.Context(), UserContextKey, user)
				ctx = context.WithValue(ctx, ClaimsContextKey, claims)
				r = r.WithContext(ctx)
			}
		}

		next.ServeHTTP(w, r)
	})
}

// logPermissionDenied logs a permission denied event
func (m *Middleware) logPermissionDenied(r *http.Request, user *User, checkType, required string) {
	m.service.logAuditEvent(r.Context(), &user.ID, user.Username, EventPermissionDenied,
		r.URL.Path, r.Method, map[string]interface{}{
			"check_type": checkType,
			"required":   required,
			"user_role":  user.RoleName,
		}, getClientIP(r), r.UserAgent(), false)
}

// logCSRFViolation logs a CSRF violation
func (m *Middleware) logCSRFViolation(r *http.Request) {
	user := GetUserFromContext(r.Context())
	var userID *int
	username := ""
	if user != nil {
		userID = &user.ID
		username = user.Username
	}

	m.service.logAuditEvent(r.Context(), userID, username, EventCSRFViolation,
		r.URL.Path, r.Method, nil, getClientIP(r), r.UserAgent(), false)
}

// GetUserFromContext extracts the user from request context
func GetUserFromContext(ctx context.Context) *User {
	if user, ok := ctx.Value(UserContextKey).(*User); ok {
		return user
	}
	return nil
}

// GetClaimsFromContext extracts JWT claims from request context
func GetClaimsFromContext(ctx context.Context) *JWTClaims {
	if claims, ok := ctx.Value(ClaimsContextKey).(*JWTClaims); ok {
		return claims
	}
	return nil
}

// setAuthCookies sets the authentication cookies
func setAuthCookies(w http.ResponseWriter, accessToken, refreshToken string, secure bool, jwtExpirySec int) {
	// Access token cookie
	http.SetCookie(w, &http.Cookie{
		Name:     AccessTokenCookie,
		Value:    accessToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   jwtExpirySec,
	})

	// Refresh token cookie (long-lived)
	http.SetCookie(w, &http.Cookie{
		Name:     RefreshTokenCookie,
		Value:    refreshToken,
		Path:     "/api/auth/refresh",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   604800, // 7 days
	})
}

// clearAuthCookies removes authentication cookies
func clearAuthCookies(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     AccessTokenCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})

	http.SetCookie(w, &http.Cookie{
		Name:     RefreshTokenCookie,
		Value:    "",
		Path:     "/api/auth/refresh",
		HttpOnly: true,
		MaxAge:   -1,
	})

	http.SetCookie(w, &http.Cookie{
		Name:   CSRFTokenCookie,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
}

// SetCSRFCookie generates and sets a CSRF token cookie
func SetCSRFCookie(w http.ResponseWriter, secure bool) string {
	token := generateCSRFToken()
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFTokenCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: false, // JavaScript needs to read this
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400, // 24 hours
	})
	return token
}

// generateCSRFToken generates a secure CSRF token
func generateCSRFToken() string {
	bytes := make([]byte, 32)
	rand.Read(bytes)
	return base64.URLEncoding.EncodeToString(bytes)
}

// getClientIP extracts the client IP from the request
func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header (for reverse proxies)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}

	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	ip := r.RemoteAddr
	if colonIdx := strings.LastIndex(ip, ":"); colonIdx != -1 {
		ip = ip[:colonIdx]
	}
	return ip
}

// RateLimiter provides rate limiting for authentication endpoints
type RateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
	limit    int
	window   time.Duration
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
}

// Allow checks if a request from the given key is allowed
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Get existing requests for this key
	requests := rl.requests[key]

	// Filter out old requests
	var recent []time.Time
	for _, t := range requests {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}

	// Check if under limit
	if len(recent) >= rl.limit {
		return false
	}

	// Add this request
	recent = append(recent, now)
	rl.requests[key] = recent

	return true
}

// Cleanup removes old entries from the rate limiter
func (rl *RateLimiter) Cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := time.Now().Add(-rl.window)
	for key, requests := range rl.requests {
		var recent []time.Time
		for _, t := range requests {
			if t.After(cutoff) {
				recent = append(recent, t)
			}
		}
		if len(recent) == 0 {
			delete(rl.requests, key)
		} else {
			rl.requests[key] = recent
		}
	}
}

// RateLimitMiddleware provides rate limiting for authentication endpoints
func (m *Middleware) RateLimitMiddleware(limiter *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := getClientIP(r)

			if !limiter.Allow(ip) {
				user := GetUserFromContext(r.Context())
				var userID *int
				username := ""
				if user != nil {
					userID = &user.ID
					username = user.Username
				}

				m.service.logAuditEvent(r.Context(), userID, username, EventRateLimitExceeded,
					r.URL.Path, r.Method, map[string]interface{}{
						"ip": ip,
					}, ip, r.UserAgent(), false)

				slog.Warn("Rate limit exceeded", "ip", ip, "path", r.URL.Path)
				http.Error(w, `{"error":"rate limit exceeded, try again later"}`, http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// SetAuthCookies is the public version for handlers
func (m *Middleware) SetAuthCookies(w http.ResponseWriter, accessToken, refreshToken string) {
	setAuthCookies(w, accessToken, refreshToken, m.config.SecureCookies, int(m.config.JWTExpiry.Seconds()))
}

// ClearAuthCookies is the public version for handlers
func (m *Middleware) ClearAuthCookies(w http.ResponseWriter) {
	clearAuthCookies(w)
}
