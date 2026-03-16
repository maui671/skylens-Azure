package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Handlers provides HTTP handlers for authentication
type Handlers struct {
	service    *Service
	middleware *Middleware
	limiter    *RateLimiter
}

// NewHandlers creates new auth handlers
func NewHandlers(service *Service, middleware *Middleware) *Handlers {
	// Create rate limiter: 10 auth requests per minute per IP
	limiter := NewRateLimiter(10, time.Minute)

	// Start cleanup goroutine
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		for range ticker.C {
			limiter.Cleanup()
		}
	}()

	return &Handlers{
		service:    service,
		middleware: middleware,
		limiter:    limiter,
	}
}

// HandleLogin processes login requests
func (h *Handlers) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Rate limit
	ip := getClientIP(r)
	if !h.limiter.Allow(ip) {
		http.Error(w, `{"error":"too many login attempts, try again later"}`, http.StatusTooManyRequests)
		return
	}

	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Validate input
	if req.Username == "" || req.Password == "" {
		http.Error(w, `{"error":"username and password are required"}`, http.StatusBadRequest)
		return
	}

	// Attempt login
	response, session, err := h.service.Login(r.Context(), &req, ip, r.UserAgent())
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidCredentials):
			http.Error(w, `{"error":"invalid username or password"}`, http.StatusUnauthorized)
		case errors.Is(err, ErrAccountLocked):
			http.Error(w, `{"error":"account is locked, try again later"}`, http.StatusForbidden)
		case errors.Is(err, ErrAccountDisabled):
			http.Error(w, `{"error":"account is disabled"}`, http.StatusForbidden)
		default:
			slog.Error("Login failed", "error", err)
			http.Error(w, `{"error":"login failed"}`, http.StatusInternalServerError)
		}
		return
	}

	// Set cookies
	h.middleware.SetAuthCookies(w, response.AccessToken, response.RefreshToken)

	// Set CSRF cookie
	SetCSRFCookie(w, h.service.config.SecureCookies)

	// Don't include tokens in response when using cookies
	response.AccessToken = ""
	response.RefreshToken = ""

	slog.Info("User logged in",
		"username", response.User.Username,
		"session_id", session.ID,
		"ip", ip,
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// HandleLogout processes logout requests
func (h *Handlers) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Get claims from context (set by RequireAuth middleware)
	claims := GetClaimsFromContext(r.Context())
	if claims != nil {
		h.service.Logout(r.Context(), claims.SessionID, getClientIP(r), r.UserAgent())
	}

	// Clear cookies
	h.middleware.ClearAuthCookies(w)

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"message":"logged out successfully"}`))
}

// HandleRefresh refreshes the access token
func (h *Handlers) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Get refresh token from cookie
	refreshCookie, err := r.Cookie(RefreshTokenCookie)
	if err != nil {
		http.Error(w, `{"error":"refresh token required"}`, http.StatusUnauthorized)
		return
	}

	// Refresh tokens
	accessToken, refreshToken, err := h.service.RefreshToken(
		r.Context(),
		refreshCookie.Value,
		getClientIP(r),
		r.UserAgent(),
	)
	if err != nil {
		h.middleware.ClearAuthCookies(w)
		http.Error(w, `{"error":"session expired, please login again"}`, http.StatusUnauthorized)
		return
	}

	// Set new cookies
	h.middleware.SetAuthCookies(w, accessToken, refreshToken)

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"message":"token refreshed"}`))
}

// HandleMe returns the current user's information
func (h *Handlers) HandleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	user := GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"user": user,
	})
}

// HandleChangePassword changes the user's password
func (h *Handlers) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	user := GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	var req ChangePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Validate new password
	if req.NewPassword != req.ConfirmPassword {
		http.Error(w, `{"error":"passwords do not match"}`, http.StatusBadRequest)
		return
	}

	if err := ValidatePassword(req.NewPassword); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
		return
	}

	// Verify current password
	if !VerifyPassword(req.CurrentPassword, user.PasswordHash) {
		http.Error(w, `{"error":"current password is incorrect"}`, http.StatusBadRequest)
		return
	}

	// Hash new password
	newHash, err := HashPassword(req.NewPassword)
	if err != nil {
		http.Error(w, `{"error":"failed to process password"}`, http.StatusInternalServerError)
		return
	}

	// Update password
	ctx := r.Context()
	_, err = h.service.pg.Exec(ctx, `
		UPDATE users SET password_hash = $2, last_password_change = $3 WHERE id = $1
	`, user.ID, newHash, time.Now())
	if err != nil {
		slog.Error("Failed to update password", "error", err)
		http.Error(w, `{"error":"failed to update password"}`, http.StatusInternalServerError)
		return
	}

	// Revoke all other sessions (security: force re-login on other devices)
	claims := GetClaimsFromContext(ctx)
	if claims != nil {
		h.service.revokeOtherSessions(ctx, user.ID, claims.SessionID)
	}

	// Log password change
	h.service.logAuditEvent(ctx, &user.ID, user.Username, EventPasswordChange, "", "", nil,
		getClientIP(r), r.UserAgent(), true)

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"message":"password changed successfully"}`))
}

// HandleListSessions lists active sessions for the current user
func (h *Handlers) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	user := GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	sessions, err := h.service.getUserSessions(r.Context(), user.ID)
	if err != nil {
		slog.Error("Failed to get sessions", "error", err)
		http.Error(w, `{"error":"failed to get sessions"}`, http.StatusInternalServerError)
		return
	}

	// Mark current session
	claims := GetClaimsFromContext(r.Context())
	currentMarker := "current"
	for i := range sessions {
		if claims != nil && sessions[i].ID == claims.SessionID {
			sessions[i].RevokedReason = &currentMarker // Use this field to mark current session
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessions": sessions,
	})
}

// HandleRevokeSession revokes a specific session
func (h *Handlers) HandleRevokeSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	user := GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	// Get session ID from URL
	sessionID := strings.TrimPrefix(r.URL.Path, "/api/auth/sessions/")
	if sessionID == "" {
		http.Error(w, `{"error":"session ID required"}`, http.StatusBadRequest)
		return
	}

	// Verify session belongs to user (unless admin)
	session, err := h.service.getSession(r.Context(), sessionID)
	if err != nil {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}

	if session.UserID != user.ID && !user.IsAdmin() {
		http.Error(w, `{"error":"permission denied"}`, http.StatusForbidden)
		return
	}

	// Revoke session
	if err := h.service.revokeSession(r.Context(), sessionID, "user_revoked"); err != nil {
		slog.Error("Failed to revoke session", "error", err)
		http.Error(w, `{"error":"failed to revoke session"}`, http.StatusInternalServerError)
		return
	}

	// Log revocation
	h.service.logAuditEvent(r.Context(), &user.ID, user.Username, EventSessionRevoked, "", "", map[string]interface{}{
		"revoked_session": sessionID,
	}, getClientIP(r), r.UserAgent(), true)

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"message":"session revoked"}`))
}

// HandleCSRFToken returns a fresh CSRF token
func (h *Handlers) HandleCSRFToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	token := SetCSRFCookie(w, h.service.config.SecureCookies)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"csrf_token": token,
	})
}

// HandlePasswordRequirements returns password policy information
func (h *Handlers) HandlePasswordRequirements(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(GetPasswordRequirements())
}

// Admin handlers

// HandleListUsers returns all users (admin only)
func (h *Handlers) HandleListUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	users, err := h.service.listUsers(r.Context())
	if err != nil {
		slog.Error("Failed to list users", "error", err)
		http.Error(w, `{"error":"failed to list users"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"users": users,
	})
}

// HandleGetUser returns a specific user (admin only)
func (h *Handlers) HandleGetUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Get user ID from URL
	userIDStr := strings.TrimPrefix(r.URL.Path, "/api/admin/users/")
	userID, err := strconv.Atoi(userIDStr)
	if err != nil {
		http.Error(w, `{"error":"invalid user ID"}`, http.StatusBadRequest)
		return
	}

	user, err := h.service.GetUserByID(r.Context(), userID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		} else {
			http.Error(w, `{"error":"failed to get user"}`, http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"user": user,
	})
}

// HandleCreateUser creates a new user (admin only)
func (h *Handlers) HandleCreateUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	admin := GetUserFromContext(r.Context())
	if admin == nil {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	var req CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Validate
	if req.Username == "" {
		http.Error(w, `{"error":"username is required"}`, http.StatusBadRequest)
		return
	}
	if req.Password == "" {
		http.Error(w, `{"error":"password is required"}`, http.StatusBadRequest)
		return
	}
	if req.RoleID == 0 {
		http.Error(w, `{"error":"role is required"}`, http.StatusBadRequest)
		return
	}

	// Validate password
	if err := ValidatePassword(req.Password); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
		return
	}

	// Create user
	user, err := h.service.createUser(r.Context(), &req, admin.ID)
	if err != nil {
		if errors.Is(err, ErrUsernameExists) {
			http.Error(w, `{"error":"username already exists"}`, http.StatusConflict)
		} else if errors.Is(err, ErrEmailExists) {
			http.Error(w, `{"error":"email already exists"}`, http.StatusConflict)
		} else {
			slog.Error("Failed to create user", "error", err)
			http.Error(w, `{"error":"failed to create user"}`, http.StatusInternalServerError)
		}
		return
	}

	// Log user creation
	h.service.logAuditEvent(r.Context(), &admin.ID, admin.Username, EventUserCreated, "users", "create",
		map[string]interface{}{
			"created_user_id":   user.ID,
			"created_username":  user.Username,
		}, getClientIP(r), r.UserAgent(), true)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"user": user,
	})
}

// HandleUpdateUser updates a user (admin only)
func (h *Handlers) HandleUpdateUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	admin := GetUserFromContext(r.Context())
	if admin == nil {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	// Get user ID from URL
	userIDStr := strings.TrimPrefix(r.URL.Path, "/api/admin/users/")
	userID, err := strconv.Atoi(userIDStr)
	if err != nil {
		http.Error(w, `{"error":"invalid user ID"}`, http.StatusBadRequest)
		return
	}

	var req UpdateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	user, err := h.service.updateUser(r.Context(), userID, &req)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		} else if errors.Is(err, ErrEmailExists) {
			http.Error(w, `{"error":"email already exists"}`, http.StatusConflict)
		} else {
			slog.Error("Failed to update user", "error", err)
			http.Error(w, `{"error":"failed to update user"}`, http.StatusInternalServerError)
		}
		return
	}

	// Log user update
	h.service.logAuditEvent(r.Context(), &admin.ID, admin.Username, EventUserUpdated, "users", "update",
		map[string]interface{}{
			"updated_user_id":   user.ID,
			"updated_username":  user.Username,
		}, getClientIP(r), r.UserAgent(), true)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"user": user,
	})
}

// HandleDeleteUser deletes a user (admin only)
func (h *Handlers) HandleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	admin := GetUserFromContext(r.Context())
	if admin == nil {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	// Get user ID from URL
	userIDStr := strings.TrimPrefix(r.URL.Path, "/api/admin/users/")
	userID, err := strconv.Atoi(userIDStr)
	if err != nil {
		http.Error(w, `{"error":"invalid user ID"}`, http.StatusBadRequest)
		return
	}

	// Can't delete yourself
	if userID == admin.ID {
		http.Error(w, `{"error":"cannot delete your own account"}`, http.StatusBadRequest)
		return
	}

	// Get username for logging
	user, _ := h.service.GetUserByID(r.Context(), userID)

	if err := h.service.deleteUser(r.Context(), userID); err != nil {
		if errors.Is(err, ErrUserNotFound) {
			http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		} else {
			slog.Error("Failed to delete user", "error", err)
			http.Error(w, `{"error":"failed to delete user"}`, http.StatusInternalServerError)
		}
		return
	}

	// Log user deletion
	username := ""
	if user != nil {
		username = user.Username
	}
	h.service.logAuditEvent(r.Context(), &admin.ID, admin.Username, EventUserDeleted, "users", "delete",
		map[string]interface{}{
			"deleted_user_id":   userID,
			"deleted_username":  username,
		}, getClientIP(r), r.UserAgent(), true)

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"message":"user deleted"}`))
}

// HandleListRoles returns all roles
func (h *Handlers) HandleListRoles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	roles, err := h.service.listRoles(r.Context())
	if err != nil {
		slog.Error("Failed to list roles", "error", err)
		http.Error(w, `{"error":"failed to list roles"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"roles": roles,
	})
}

// HandleResetPassword resets a user's password (admin only)
func (h *Handlers) HandleResetPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	admin := GetUserFromContext(r.Context())
	if admin == nil {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	// Get user ID from URL
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/users/")
	path = strings.TrimSuffix(path, "/reset-password")
	userID, err := strconv.Atoi(path)
	if err != nil {
		http.Error(w, `{"error":"invalid user ID"}`, http.StatusBadRequest)
		return
	}

	var req struct {
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if err := ValidatePassword(req.NewPassword); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
		return
	}

	// Hash and update password
	hash, err := HashPassword(req.NewPassword)
	if err != nil {
		http.Error(w, `{"error":"failed to process password"}`, http.StatusInternalServerError)
		return
	}

	_, err = h.service.pg.Exec(r.Context(), `
		UPDATE users SET password_hash = $2, last_password_change = $3 WHERE id = $1
	`, userID, hash, time.Now())
	if err != nil {
		slog.Error("Failed to reset password", "error", err)
		http.Error(w, `{"error":"failed to reset password"}`, http.StatusInternalServerError)
		return
	}

	// Revoke all sessions for this user
	h.service.revokeAllUserSessions(r.Context(), userID)

	// Log password reset
	user, _ := h.service.GetUserByID(r.Context(), userID)
	username := ""
	if user != nil {
		username = user.Username
	}
	h.service.logAuditEvent(r.Context(), &admin.ID, admin.Username, EventPasswordReset, "users", "reset_password",
		map[string]interface{}{
			"reset_user_id":   userID,
			"reset_username":  username,
		}, getClientIP(r), r.UserAgent(), true)

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"message":"password reset successfully"}`))
}

// Service helper methods

func (s *Service) getUserSessions(ctx context.Context, userID int) ([]Session, error) {
	rows, err := s.pg.Query(ctx, `
		SELECT id, user_id, ip_address, user_agent, created_at, expires_at, last_activity, is_revoked
		FROM sessions
		WHERE user_id = $1 AND is_revoked = false AND expires_at > NOW()
		ORDER BY last_activity DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.UserID, &s.IPAddress, &s.UserAgent,
			&s.CreatedAt, &s.ExpiresAt, &s.LastActivity, &s.IsRevoked); err != nil {
			continue
		}
		sessions = append(sessions, s)
	}

	return sessions, nil
}

func (s *Service) revokeOtherSessions(ctx context.Context, userID int, currentSessionID string) {
	s.pg.Exec(ctx, `
		UPDATE sessions SET is_revoked = true, revoked_at = $3, revoked_reason = 'password_change'
		WHERE user_id = $1 AND id != $2 AND is_revoked = false
	`, userID, currentSessionID, time.Now())
}

func (s *Service) revokeAllUserSessions(ctx context.Context, userID int) {
	s.pg.Exec(ctx, `
		UPDATE sessions SET is_revoked = true, revoked_at = $2, revoked_reason = 'admin_reset'
		WHERE user_id = $1 AND is_revoked = false
	`, userID, time.Now())
}

func (s *Service) listUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pg.Query(ctx, `
		SELECT u.id, u.username, u.email, u.display_name,
			   u.role_id, r.name as role_name, u.is_active, u.is_locked,
			   u.last_login, u.created_at
		FROM users u
		JOIN roles r ON u.role_id = r.id
		ORDER BY u.username
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &u.DisplayName,
			&u.RoleID, &u.RoleName, &u.IsActive, &u.IsLocked,
			&u.LastLogin, &u.CreatedAt); err != nil {
			continue
		}
		// Load allowed TAPs
		u.AllowedTaps, _ = s.getUserAllowedTaps(ctx, u.ID)
		users = append(users, u)
	}

	return users, nil
}

func (s *Service) listRoles(ctx context.Context) ([]Role, error) {
	rows, err := s.pg.Query(ctx, `
		SELECT id, name, description, is_system, created_at
		FROM roles
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var roles []Role
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.ID, &r.Name, &r.Description, &r.IsSystem, &r.CreatedAt); err != nil {
			continue
		}
		roles = append(roles, r)
	}

	return roles, nil
}

func (s *Service) createUser(ctx context.Context, req *CreateUserRequest, createdBy int) (*User, error) {
	// Check username doesn't exist
	var exists int
	err := s.pg.QueryRow(ctx, `SELECT 1 FROM users WHERE username = $1`, req.Username).Scan(&exists)
	if err == nil {
		return nil, ErrUsernameExists
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// Check email doesn't exist (if provided)
	if req.Email != "" {
		err = s.pg.QueryRow(ctx, `SELECT 1 FROM users WHERE email = $1`, req.Email).Scan(&exists)
		if err == nil {
			return nil, ErrEmailExists
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}

	// Hash password
	hash, err := HashPassword(req.Password)
	if err != nil {
		return nil, err
	}

	// Insert user
	var userID int
	err = s.pg.QueryRow(ctx, `
		INSERT INTO users (username, email, password_hash, display_name, role_id, is_active, created_by)
		VALUES ($1, $2, $3, $4, $5, true, $6)
		RETURNING id
	`, req.Username, req.Email, hash, req.DisplayName, req.RoleID, createdBy).Scan(&userID)
	if err != nil {
		return nil, err
	}

	// Add TAP access if provided
	if len(req.AllowedTaps) > 0 {
		for _, tapID := range req.AllowedTaps {
			s.pg.Exec(ctx, `
				INSERT INTO user_tap_access (user_id, tap_id, granted_by)
				VALUES ($1, $2, $3)
			`, userID, tapID, createdBy)
		}
	}

	return s.GetUserByID(ctx, userID)
}

func (s *Service) updateUser(ctx context.Context, userID int, req *UpdateUserRequest) (*User, error) {
	// Check user exists
	_, err := s.GetUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}

	// Check email uniqueness if changing
	if req.Email != nil && *req.Email != "" {
		var existingID int
		err := s.pg.QueryRow(ctx, `SELECT id FROM users WHERE email = $1 AND id != $2`, *req.Email, userID).Scan(&existingID)
		if err == nil {
			return nil, ErrEmailExists
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}

	// Build update query dynamically
	updates := []string{}
	args := []interface{}{userID}
	argIdx := 2

	if req.Email != nil {
		updates = append(updates, fmt.Sprintf("email = $%d", argIdx))
		args = append(args, *req.Email)
		argIdx++
	}
	if req.DisplayName != nil {
		updates = append(updates, fmt.Sprintf("display_name = $%d", argIdx))
		args = append(args, *req.DisplayName)
		argIdx++
	}
	if req.RoleID != nil {
		updates = append(updates, fmt.Sprintf("role_id = $%d", argIdx))
		args = append(args, *req.RoleID)
		argIdx++
	}
	if req.IsActive != nil {
		updates = append(updates, fmt.Sprintf("is_active = $%d", argIdx))
		args = append(args, *req.IsActive)
		argIdx++
	}

	if len(updates) > 0 {
		updates = append(updates, "updated_at = NOW()")
		query := fmt.Sprintf("UPDATE users SET %s WHERE id = $1", strings.Join(updates, ", "))
		_, err = s.pg.Exec(ctx, query, args...)
		if err != nil {
			return nil, err
		}
	}

	// Update TAP access
	if req.AllowedTaps != nil {
		// Remove existing
		s.pg.Exec(ctx, `DELETE FROM user_tap_access WHERE user_id = $1`, userID)

		// Add new
		for _, tapID := range req.AllowedTaps {
			s.pg.Exec(ctx, `
				INSERT INTO user_tap_access (user_id, tap_id)
				VALUES ($1, $2)
			`, userID, tapID)
		}
	}

	return s.GetUserByID(ctx, userID)
}

func (s *Service) deleteUser(ctx context.Context, userID int) error {
	result, err := s.pg.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}
