package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Common errors
var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrAccountLocked      = errors.New("account is locked due to too many failed attempts")
	ErrAccountDisabled    = errors.New("account is disabled")
	ErrSessionExpired     = errors.New("session has expired")
	ErrSessionRevoked     = errors.New("session has been revoked")
	ErrInvalidToken       = errors.New("invalid or expired token")
	ErrUserNotFound       = errors.New("user not found")
	ErrUsernameExists     = errors.New("username already exists")
	ErrEmailExists        = errors.New("email already exists")
	ErrPermissionDenied   = errors.New("permission denied")
)

// Config holds authentication service configuration
type Config struct {
	JWTSecret           string        `yaml:"jwt_secret"`
	JWTExpiry           time.Duration `yaml:"jwt_expiry"`           // Access token expiry (default 8 hours)
	RefreshExpiry       time.Duration `yaml:"refresh_expiry"`       // Refresh token expiry (default 7 days)
	SessionExpiry       time.Duration `yaml:"session_expiry"`       // Session expiry (default 24 hours)
	MaxFailedAttempts   int           `yaml:"max_failed_attempts"`  // Account lockout threshold
	LockoutDuration     time.Duration `yaml:"lockout_duration"`     // Account lockout duration
	SecureCookies       bool          `yaml:"secure_cookies"`       // Use secure cookies (HTTPS only)
	CSRFEnabled         bool          `yaml:"csrf_enabled"`         // Enable CSRF protection
}

// DefaultConfig returns default auth configuration
func DefaultConfig() Config {
	return Config{
		JWTSecret:         "", // Must be set via environment variable
		JWTExpiry:         8 * time.Hour,
		RefreshExpiry:     7 * 24 * time.Hour,
		SessionExpiry:     24 * time.Hour,
		MaxFailedAttempts: 5,
		LockoutDuration:   15 * time.Minute,
		SecureCookies:     false, // Set to true in production with HTTPS
		CSRFEnabled:       true,
	}
}

// Service provides authentication operations
type Service struct {
	config Config
	pg     *pgxpool.Pool
	redis  *redis.Client
	jwt    *JWTManager
	ctx    context.Context
}

// NewService creates a new authentication service
func NewService(ctx context.Context, config Config, pg *pgxpool.Pool, redis *redis.Client) (*Service, error) {
	// Validate config
	if config.JWTSecret == "" {
		return nil, errors.New("JWT secret is required")
	}

	jwt := NewJWTManager(config.JWTSecret, config.JWTExpiry, config.RefreshExpiry)

	return &Service{
		config: config,
		pg:     pg,
		redis:  redis,
		jwt:    jwt,
		ctx:    ctx,
	}, nil
}

// Login authenticates a user and creates a session
func (s *Service) Login(ctx context.Context, req *LoginRequest, ipAddress, userAgent string) (*LoginResponse, *Session, error) {
	// Get user by username
	user, err := s.GetUserByUsername(ctx, req.Username)
	if err != nil {
		// Log failed attempt without revealing if user exists
		s.logAuditEvent(ctx, nil, req.Username, EventLoginFailed, "", "", map[string]interface{}{
			"reason": "invalid_credentials",
		}, ipAddress, userAgent, false)
		return nil, nil, ErrInvalidCredentials
	}

	// Check if account is locked
	if user.IsLocked {
		if user.LockedUntil != nil && time.Now().Before(*user.LockedUntil) {
			s.logAuditEvent(ctx, &user.ID, user.Username, EventLoginFailed, "", "", map[string]interface{}{
				"reason":       "account_locked",
				"locked_until": user.LockedUntil,
			}, ipAddress, userAgent, false)
			return nil, nil, ErrAccountLocked
		}
		// Lockout expired, unlock account
		s.unlockAccount(ctx, user.ID)
		user.IsLocked = false
	}

	// Check if account is active
	if !user.IsActive {
		s.logAuditEvent(ctx, &user.ID, user.Username, EventLoginFailed, "", "", map[string]interface{}{
			"reason": "account_disabled",
		}, ipAddress, userAgent, false)
		return nil, nil, ErrAccountDisabled
	}

	// Verify password
	if !VerifyPassword(req.Password, user.PasswordHash) {
		// Increment failed attempts
		s.incrementFailedAttempts(ctx, user.ID)
		user.FailedAttempts++

		// Check if we need to lock the account
		if user.FailedAttempts >= s.config.MaxFailedAttempts {
			s.lockAccount(ctx, user.ID, s.config.LockoutDuration)
			s.logAuditEvent(ctx, &user.ID, user.Username, EventAccountLocked, "", "", map[string]interface{}{
				"failed_attempts": user.FailedAttempts,
				"lockout_minutes": s.config.LockoutDuration.Minutes(),
			}, ipAddress, userAgent, true)
		}

		s.logAuditEvent(ctx, &user.ID, user.Username, EventLoginFailed, "", "", map[string]interface{}{
			"reason":          "invalid_password",
			"failed_attempts": user.FailedAttempts,
		}, ipAddress, userAgent, false)

		return nil, nil, ErrInvalidCredentials
	}

	// Reset failed attempts on successful login
	s.resetFailedAttempts(ctx, user.ID)

	// Create session
	session, err := s.createSession(ctx, user, ipAddress, userAgent, req.RememberMe)
	if err != nil {
		return nil, nil, fmt.Errorf("create session: %w", err)
	}

	// Generate tokens
	accessToken, err := s.jwt.GenerateAccessToken(user, session.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("generate access token: %w", err)
	}

	refreshToken, err := s.jwt.GenerateRefreshToken(user, session.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("generate refresh token: %w", err)
	}

	// Store refresh token hash in session
	session.RefreshToken = refreshToken
	s.updateSessionRefreshToken(ctx, session.ID, refreshToken)

	// Update last login
	s.updateLastLogin(ctx, user.ID)

	// Log successful login
	s.logAuditEvent(ctx, &user.ID, user.Username, EventLogin, "", "", map[string]interface{}{
		"session_id": session.ID,
	}, ipAddress, userAgent, true)

	return &LoginResponse{
		User:         user,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    int(s.config.JWTExpiry.Seconds()),
		TokenType:    "Bearer",
	}, session, nil
}

// Logout invalidates a session
func (s *Service) Logout(ctx context.Context, sessionID string, ipAddress, userAgent string) error {
	// Get session to find user
	session, err := s.getSession(ctx, sessionID)
	if err != nil {
		return nil // Session doesn't exist, consider it logged out
	}

	// Revoke session
	if err := s.revokeSession(ctx, sessionID, "logout"); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}

	// Remove from Redis cache
	if s.redis != nil {
		s.redis.Del(ctx, "session:"+sessionID)
	}

	// Log logout
	s.logAuditEvent(ctx, &session.UserID, "", EventLogout, "", "", map[string]interface{}{
		"session_id": sessionID,
	}, ipAddress, userAgent, true)

	return nil
}

// ValidateToken validates an access token and returns the user
func (s *Service) ValidateToken(ctx context.Context, tokenString string) (*User, *JWTClaims, error) {
	// Parse and validate token
	slog.Debug("Service.ValidateToken called", "token_length", len(tokenString))
	claims, err := s.jwt.ValidateToken(tokenString)
	if err != nil {
		slog.Debug("JWT validation failed", "error", err)
		return nil, nil, ErrInvalidToken
	}
	slog.Debug("JWT validated successfully", "session_id", claims.SessionID, "user_id", claims.UserID)

	// Check token type
	if claims.TokenType != "access" {
		slog.Debug("Token type mismatch", "type", claims.TokenType)
		return nil, nil, ErrInvalidToken
	}

	// Check session is still valid
	session, err := s.getSession(ctx, claims.SessionID)
	if err != nil {
		slog.Debug("Session lookup failed", "session_id", claims.SessionID, "error", err)
		return nil, nil, ErrSessionExpired
	}

	if session.IsRevoked {
		slog.Debug("Session is revoked", "session_id", claims.SessionID)
		return nil, nil, ErrSessionRevoked
	}

	if session.IsExpired() {
		slog.Debug("Session is expired", "session_id", claims.SessionID)
		return nil, nil, ErrSessionExpired
	}

	// Get fresh user data
	user, err := s.GetUserByID(ctx, claims.UserID)
	if err != nil {
		slog.Debug("User lookup failed", "user_id", claims.UserID, "error", err)
		return nil, nil, ErrUserNotFound
	}

	// Check if user is still active
	if !user.IsActive {
		slog.Debug("User not active", "user_id", claims.UserID)
		return nil, nil, ErrAccountDisabled
	}

	// Update session last activity
	s.updateSessionActivity(ctx, claims.SessionID)

	return user, claims, nil
}

// RefreshToken creates a new access token using a refresh token
func (s *Service) RefreshToken(ctx context.Context, refreshTokenString, ipAddress, userAgent string) (string, string, error) {
	// Parse and validate refresh token
	claims, err := s.jwt.ValidateToken(refreshTokenString)
	if err != nil {
		return "", "", ErrInvalidToken
	}

	// Check token type
	if claims.TokenType != "refresh" {
		return "", "", ErrInvalidToken
	}

	// Get session
	session, err := s.getSession(ctx, claims.SessionID)
	if err != nil {
		return "", "", ErrSessionExpired
	}

	if session.IsRevoked {
		return "", "", ErrSessionRevoked
	}

	// Get user
	user, err := s.GetUserByID(ctx, claims.UserID)
	if err != nil {
		return "", "", ErrUserNotFound
	}

	if !user.IsActive {
		return "", "", ErrAccountDisabled
	}

	// Generate new access token
	accessToken, err := s.jwt.GenerateAccessToken(user, session.ID)
	if err != nil {
		return "", "", fmt.Errorf("generate access token: %w", err)
	}

	// Generate new refresh token (rotation)
	newRefreshToken, err := s.jwt.GenerateRefreshToken(user, session.ID)
	if err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}

	// Update session
	s.updateSessionRefreshToken(ctx, session.ID, newRefreshToken)
	s.updateSessionActivity(ctx, session.ID)

	// Log refresh
	s.logAuditEvent(ctx, &user.ID, user.Username, EventTokenRefresh, "", "", map[string]interface{}{
		"session_id": session.ID,
	}, ipAddress, userAgent, true)

	return accessToken, newRefreshToken, nil
}

// GetUserByID retrieves a user by their ID
func (s *Service) GetUserByID(ctx context.Context, id int) (*User, error) {
	user := &User{}
	err := s.pg.QueryRow(ctx, `
		SELECT u.id, u.username, u.email, u.password_hash, u.display_name,
			   u.role_id, r.name as role_name, u.is_active, u.is_locked, u.locked_until,
			   u.failed_attempts, u.last_login, u.last_password_change,
			   u.created_at, u.updated_at, u.created_by
		FROM users u
		JOIN roles r ON u.role_id = r.id
		WHERE u.id = $1
	`, id).Scan(
		&user.ID, &user.Username, &user.Email, &user.PasswordHash, &user.DisplayName,
		&user.RoleID, &user.RoleName, &user.IsActive, &user.IsLocked, &user.LockedUntil,
		&user.FailedAttempts, &user.LastLogin, &user.LastPasswordChange,
		&user.CreatedAt, &user.UpdatedAt, &user.CreatedBy,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("get user: %w", err)
	}

	// Load permissions
	user.Permissions, _ = s.getUserPermissions(ctx, user.ID)

	// Load allowed TAPs
	user.AllowedTaps, _ = s.getUserAllowedTaps(ctx, user.ID)

	return user, nil
}

// GetUserByUsername retrieves a user by username
func (s *Service) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	user := &User{}
	err := s.pg.QueryRow(ctx, `
		SELECT u.id, u.username, u.email, u.password_hash, u.display_name,
			   u.role_id, r.name as role_name, u.is_active, u.is_locked, u.locked_until,
			   u.failed_attempts, u.last_login, u.last_password_change,
			   u.created_at, u.updated_at, u.created_by
		FROM users u
		JOIN roles r ON u.role_id = r.id
		WHERE u.username = $1
	`, username).Scan(
		&user.ID, &user.Username, &user.Email, &user.PasswordHash, &user.DisplayName,
		&user.RoleID, &user.RoleName, &user.IsActive, &user.IsLocked, &user.LockedUntil,
		&user.FailedAttempts, &user.LastLogin, &user.LastPasswordChange,
		&user.CreatedAt, &user.UpdatedAt, &user.CreatedBy,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("get user: %w", err)
	}

	// Load permissions
	user.Permissions, _ = s.getUserPermissions(ctx, user.ID)

	// Load allowed TAPs
	user.AllowedTaps, _ = s.getUserAllowedTaps(ctx, user.ID)

	return user, nil
}

// getUserPermissions loads all permissions for a user's role
func (s *Service) getUserPermissions(ctx context.Context, userID int) ([]string, error) {
	rows, err := s.pg.Query(ctx, `
		SELECT p.name
		FROM permissions p
		JOIN role_permissions rp ON p.id = rp.permission_id
		JOIN users u ON u.role_id = rp.role_id
		WHERE u.id = $1
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var perms []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		perms = append(perms, name)
	}

	return perms, nil
}

// getUserAllowedTaps loads allowed TAPs for a user
func (s *Service) getUserAllowedTaps(ctx context.Context, userID int) ([]string, error) {
	rows, err := s.pg.Query(ctx, `
		SELECT tap_id FROM user_tap_access WHERE user_id = $1
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var taps []string
	for rows.Next() {
		var tapID string
		if err := rows.Scan(&tapID); err != nil {
			continue
		}
		taps = append(taps, tapID)
	}

	return taps, nil
}

// createSession creates a new session for a user
func (s *Service) createSession(ctx context.Context, user *User, ipAddress, userAgent string, rememberMe bool) (*Session, error) {
	sessionID, err := generateSecureID(32)
	if err != nil {
		return nil, fmt.Errorf("generate session ID: %w", err)
	}

	expiry := s.config.SessionExpiry
	if rememberMe {
		expiry = s.config.RefreshExpiry // Extended session for "remember me"
	}
	expiresAt := time.Now().Add(expiry)

	session := &Session{
		ID:           sessionID,
		UserID:       user.ID,
		IPAddress:    ipAddress,
		UserAgent:    userAgent,
		CreatedAt:    time.Now(),
		ExpiresAt:    expiresAt,
		LastActivity: time.Now(),
	}

	_, err = s.pg.Exec(ctx, `
		INSERT INTO sessions (id, user_id, ip_address, user_agent, created_at, expires_at, last_activity)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, session.ID, session.UserID, session.IPAddress, session.UserAgent,
		session.CreatedAt, session.ExpiresAt, session.LastActivity)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}

	// Cache session in Redis
	if s.redis != nil {
		s.redis.Set(ctx, "session:"+sessionID, session.UserID, expiry)
	}

	return session, nil
}

// getSession retrieves a session by ID
func (s *Service) getSession(ctx context.Context, sessionID string) (*Session, error) {
	session := &Session{}
	err := s.pg.QueryRow(ctx, `
		SELECT id, user_id, refresh_token, ip_address, user_agent,
			   created_at, expires_at, last_activity, is_revoked, revoked_at, revoked_reason
		FROM sessions
		WHERE id = $1
	`, sessionID).Scan(
		&session.ID, &session.UserID, &session.RefreshToken, &session.IPAddress,
		&session.UserAgent, &session.CreatedAt, &session.ExpiresAt, &session.LastActivity,
		&session.IsRevoked, &session.RevokedAt, &session.RevokedReason,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSessionExpired
		}
		return nil, fmt.Errorf("get session: %w", err)
	}

	return session, nil
}

// revokeSession marks a session as revoked
func (s *Service) revokeSession(ctx context.Context, sessionID, reason string) error {
	now := time.Now()
	_, err := s.pg.Exec(ctx, `
		UPDATE sessions SET is_revoked = true, revoked_at = $2, revoked_reason = $3
		WHERE id = $1
	`, sessionID, now, reason)
	return err
}

// updateSessionRefreshToken updates the refresh token for a session
func (s *Service) updateSessionRefreshToken(ctx context.Context, sessionID, refreshToken string) {
	s.pg.Exec(ctx, `
		UPDATE sessions SET refresh_token = $2 WHERE id = $1
	`, sessionID, refreshToken)
}

// updateSessionActivity updates the last activity time for a session
func (s *Service) updateSessionActivity(ctx context.Context, sessionID string) {
	s.pg.Exec(ctx, `
		UPDATE sessions SET last_activity = $2 WHERE id = $1
	`, sessionID, time.Now())
}

// updateLastLogin updates the user's last login timestamp
func (s *Service) updateLastLogin(ctx context.Context, userID int) {
	s.pg.Exec(ctx, `
		UPDATE users SET last_login = $2 WHERE id = $1
	`, userID, time.Now())
}

// incrementFailedAttempts increments the failed login attempts counter
func (s *Service) incrementFailedAttempts(ctx context.Context, userID int) {
	s.pg.Exec(ctx, `
		UPDATE users SET failed_attempts = failed_attempts + 1 WHERE id = $1
	`, userID)
}

// resetFailedAttempts resets the failed login attempts counter
func (s *Service) resetFailedAttempts(ctx context.Context, userID int) {
	s.pg.Exec(ctx, `
		UPDATE users SET failed_attempts = 0 WHERE id = $1
	`, userID)
}

// lockAccount locks a user account
func (s *Service) lockAccount(ctx context.Context, userID int, duration time.Duration) {
	lockedUntil := time.Now().Add(duration)
	s.pg.Exec(ctx, `
		UPDATE users SET is_locked = true, locked_until = $2 WHERE id = $1
	`, userID, lockedUntil)
}

// unlockAccount unlocks a user account
func (s *Service) unlockAccount(ctx context.Context, userID int) {
	s.pg.Exec(ctx, `
		UPDATE users SET is_locked = false, locked_until = NULL, failed_attempts = 0 WHERE id = $1
	`, userID)
}

// logAuditEvent records a security event
func (s *Service) logAuditEvent(ctx context.Context, userID *int, username, eventType, resource, action string, details map[string]interface{}, ipAddress, userAgent string, success bool) {
	_, err := s.pg.Exec(ctx, `
		INSERT INTO audit_log (user_id, username, event_type, resource, action, details, ip_address, user_agent, success)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, userID, username, eventType, resource, action, details, ipAddress, userAgent, success)

	if err != nil {
		slog.Warn("Failed to log audit event", "error", err, "event_type", eventType)
	}
}

// generateSecureID generates a cryptographically secure random ID
func generateSecureID(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(bytes)[:length], nil
}

// GetConfig returns the auth configuration
func (s *Service) GetConfig() Config {
	return s.config
}
