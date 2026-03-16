package auth

import (
	"time"
)

// User represents a system user
type User struct {
	ID                  int        `json:"id"`
	Username            string     `json:"username"`
	Email               string     `json:"email,omitempty"`
	PasswordHash        string     `json:"-"` // Never expose password hash
	DisplayName         string     `json:"display_name,omitempty"`
	RoleID              int        `json:"role_id"`
	RoleName            string     `json:"role_name"`
	IsActive            bool       `json:"is_active"`
	IsLocked            bool       `json:"is_locked"`
	LockedUntil         *time.Time `json:"locked_until,omitempty"`
	FailedAttempts      int        `json:"-"`
	LastLogin           *time.Time `json:"last_login,omitempty"`
	LastPasswordChange  *time.Time `json:"last_password_change,omitempty"`
	PasswordResetToken  string     `json:"-"`
	PasswordResetExpires *time.Time `json:"-"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	CreatedBy           *int       `json:"created_by,omitempty"`
	Permissions         []string   `json:"permissions,omitempty"`
	AllowedTaps         []string   `json:"allowed_taps,omitempty"` // TAPs this user can access (for Viewer role)
}

// Role represents a permission set
type Role struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	IsSystem    bool      `json:"is_system"`
	CreatedAt   time.Time `json:"created_at"`
	Permissions []string  `json:"permissions,omitempty"`
}

// Permission represents a single permission
type Permission struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Resource    string    `json:"resource"`
	Action      string    `json:"action"`
	CreatedAt   time.Time `json:"created_at"`
}

// Session represents an active user session
type Session struct {
	ID            string     `json:"id"`
	UserID        int        `json:"user_id"`
	RefreshToken  string     `json:"-"`
	IPAddress     string     `json:"ip_address,omitempty"`
	UserAgent     string     `json:"user_agent,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	LastActivity  time.Time  `json:"last_activity"`
	IsRevoked     bool       `json:"is_revoked"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	RevokedReason *string    `json:"revoked_reason,omitempty"`
}

// JWTClaims contains the claims stored in JWT tokens
type JWTClaims struct {
	UserID      int      `json:"uid"`
	Username    string   `json:"username"`
	RoleID      int      `json:"role_id"`
	RoleName    string   `json:"role"`
	SessionID   string   `json:"sid"`
	Permissions []string `json:"perms,omitempty"`
	IssuedAt    int64    `json:"iat"`
	ExpiresAt   int64    `json:"exp"`
	NotBefore   int64    `json:"nbf,omitempty"`
	TokenType   string   `json:"type"` // "access" or "refresh"
}

// AuditEvent represents a security audit log entry
type AuditEvent struct {
	ID        int                    `json:"id"`
	Timestamp time.Time              `json:"timestamp"`
	UserID    *int                   `json:"user_id,omitempty"`
	Username  string                 `json:"username,omitempty"`
	EventType string                 `json:"event_type"`
	Resource  string                 `json:"resource,omitempty"`
	Action    string                 `json:"action,omitempty"`
	Details   map[string]interface{} `json:"details,omitempty"`
	IPAddress string                 `json:"ip_address,omitempty"`
	UserAgent string                 `json:"user_agent,omitempty"`
	Success   bool                   `json:"success"`
}

// LoginRequest represents a login attempt
type LoginRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	RememberMe bool   `json:"remember_me"`
}

// LoginResponse contains the response after successful login
type LoginResponse struct {
	User         *User  `json:"user"`
	AccessToken  string `json:"access_token,omitempty"` // Only included if not using cookies
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in"` // Seconds until access token expires
	TokenType    string `json:"token_type"` // "Bearer"
}

// ChangePasswordRequest contains password change data
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
}

// CreateUserRequest contains data for creating a new user
type CreateUserRequest struct {
	Username    string   `json:"username"`
	Email       string   `json:"email,omitempty"`
	Password    string   `json:"password"`
	DisplayName string   `json:"display_name,omitempty"`
	RoleID      int      `json:"role_id"`
	AllowedTaps []string `json:"allowed_taps,omitempty"`
}

// UpdateUserRequest contains data for updating a user
type UpdateUserRequest struct {
	Email       *string  `json:"email,omitempty"`
	DisplayName *string  `json:"display_name,omitempty"`
	RoleID      *int     `json:"role_id,omitempty"`
	IsActive    *bool    `json:"is_active,omitempty"`
	AllowedTaps []string `json:"allowed_taps,omitempty"`
}

// Event types for audit logging
const (
	EventLogin              = "login"
	EventLoginFailed        = "login_failed"
	EventLogout             = "logout"
	EventTokenRefresh       = "token_refresh"
	EventPasswordChange     = "password_change"
	EventPasswordReset      = "password_reset"
	EventAccountLocked      = "account_locked"
	EventAccountUnlocked    = "account_unlock"
	EventUserCreated        = "user_created"
	EventUserUpdated        = "user_updated"
	EventUserDeleted        = "user_deleted"
	EventSessionRevoked     = "session_revoked"
	EventPermissionDenied   = "permission_denied"
	EventCSRFViolation      = "csrf_violation"
	EventRateLimitExceeded  = "rate_limit_exceeded"
)

// HasPermission checks if the user has a specific permission
func (u *User) HasPermission(permission string) bool {
	for _, p := range u.Permissions {
		if p == permission {
			return true
		}
	}
	return false
}

// HasRole checks if the user has a specific role
func (u *User) HasRole(roleName string) bool {
	return u.RoleName == roleName
}

// IsAdmin checks if the user is an admin
func (u *User) IsAdmin() bool {
	return u.HasRole("admin")
}

// CanAccessTap checks if the user can access a specific TAP
// Admins and operators can access all TAPs
// Viewers can only access their assigned TAPs
func (u *User) CanAccessTap(tapID string) bool {
	// Admins and operators can access all TAPs
	if u.RoleName == "admin" || u.RoleName == "operator" {
		return true
	}

	// Viewers must have the TAP in their allowed list
	for _, t := range u.AllowedTaps {
		if t == tapID {
			return true
		}
	}
	return false
}

// IsExpired checks if the session has expired
func (s *Session) IsExpired() bool {
	return time.Now().After(s.ExpiresAt)
}

// IsValid checks if the session is valid (not expired and not revoked)
func (s *Session) IsValid() bool {
	return !s.IsExpired() && !s.IsRevoked
}
