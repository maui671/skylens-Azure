package storage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// EnsureAuthSchema creates authentication tables if they don't exist
func (s *Store) EnsureAuthSchema() error {
	if s.pg == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()

	schema := `
	-- Roles table: defines permission sets
	CREATE TABLE IF NOT EXISTS roles (
		id SERIAL PRIMARY KEY,
		name TEXT UNIQUE NOT NULL,
		description TEXT,
		is_system BOOLEAN DEFAULT FALSE,
		created_at TIMESTAMPTZ DEFAULT NOW()
	);

	-- Permissions table: individual permissions
	CREATE TABLE IF NOT EXISTS permissions (
		id SERIAL PRIMARY KEY,
		name TEXT UNIQUE NOT NULL,
		description TEXT,
		resource TEXT NOT NULL,
		action TEXT NOT NULL,
		created_at TIMESTAMPTZ DEFAULT NOW()
	);

	-- Role-Permission mapping
	CREATE TABLE IF NOT EXISTS role_permissions (
		role_id INTEGER REFERENCES roles(id) ON DELETE CASCADE,
		permission_id INTEGER REFERENCES permissions(id) ON DELETE CASCADE,
		PRIMARY KEY (role_id, permission_id)
	);

	-- Users table
	CREATE TABLE IF NOT EXISTS users (
		id SERIAL PRIMARY KEY,
		username TEXT UNIQUE NOT NULL,
		email TEXT UNIQUE,
		password_hash TEXT NOT NULL,
		display_name TEXT,
		role_id INTEGER REFERENCES roles(id),
		is_active BOOLEAN DEFAULT TRUE,
		is_locked BOOLEAN DEFAULT FALSE,
		locked_until TIMESTAMPTZ,
		failed_attempts INTEGER DEFAULT 0,
		last_login TIMESTAMPTZ,
		last_password_change TIMESTAMPTZ DEFAULT NOW(),
		password_reset_token TEXT,
		password_reset_expires TIMESTAMPTZ,
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW(),
		created_by INTEGER REFERENCES users(id)
	);

	-- User TAP access: which TAPs a user can view (for Viewer role)
	CREATE TABLE IF NOT EXISTS user_tap_access (
		user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
		tap_id TEXT NOT NULL,
		granted_at TIMESTAMPTZ DEFAULT NOW(),
		granted_by INTEGER REFERENCES users(id),
		PRIMARY KEY (user_id, tap_id)
	);

	-- Sessions table: active user sessions
	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
		refresh_token TEXT UNIQUE,
		ip_address TEXT,
		user_agent TEXT,
		created_at TIMESTAMPTZ DEFAULT NOW(),
		expires_at TIMESTAMPTZ NOT NULL,
		last_activity TIMESTAMPTZ DEFAULT NOW(),
		is_revoked BOOLEAN DEFAULT FALSE,
		revoked_at TIMESTAMPTZ,
		revoked_reason TEXT
	);

	-- Audit log: security events
	CREATE TABLE IF NOT EXISTS audit_log (
		id SERIAL PRIMARY KEY,
		timestamp TIMESTAMPTZ DEFAULT NOW(),
		user_id INTEGER REFERENCES users(id),
		username TEXT,
		event_type TEXT NOT NULL,
		resource TEXT,
		action TEXT,
		details JSONB,
		ip_address TEXT,
		user_agent TEXT,
		success BOOLEAN DEFAULT TRUE
	);

	-- Indexes for auth tables
	CREATE INDEX IF NOT EXISTS idx_users_username ON users (username);
	CREATE INDEX IF NOT EXISTS idx_users_email ON users (email);
	CREATE INDEX IF NOT EXISTS idx_users_role ON users (role_id);
	CREATE INDEX IF NOT EXISTS idx_users_active ON users (is_active);
	CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions (user_id);
	CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions (expires_at);
	CREATE INDEX IF NOT EXISTS idx_sessions_refresh ON sessions (refresh_token);
	CREATE INDEX IF NOT EXISTS idx_sessions_revoked ON sessions (is_revoked) WHERE is_revoked = false;
	CREATE INDEX IF NOT EXISTS idx_audit_user ON audit_log (user_id);
	CREATE INDEX IF NOT EXISTS idx_audit_timestamp ON audit_log (timestamp DESC);
	CREATE INDEX IF NOT EXISTS idx_audit_event_type ON audit_log (event_type);
	CREATE INDEX IF NOT EXISTS idx_user_tap_access_user ON user_tap_access (user_id);
	CREATE INDEX IF NOT EXISTS idx_user_tap_access_tap ON user_tap_access (tap_id);
	`

	_, err := s.pg.Exec(ctx, schema)
	if err != nil {
		return fmt.Errorf("create auth schema: %w", err)
	}

	// Migrations (idempotent)
	_, err = s.pg.Exec(ctx, `ALTER TABLE users ADD COLUMN IF NOT EXISTS preferences JSONB DEFAULT '{}'::jsonb`)
	if err != nil {
		return fmt.Errorf("add preferences column: %w", err)
	}

	// Seed default roles and permissions
	if err := s.seedAuthData(ctx); err != nil {
		return fmt.Errorf("seed auth data: %w", err)
	}

	return nil
}

// seedAuthData creates default roles, permissions, and admin user
func (s *Store) seedAuthData(ctx context.Context) error {
	// Check if roles already exist
	var roleCount int
	err := s.pg.QueryRow(ctx, `SELECT COUNT(*) FROM roles`).Scan(&roleCount)
	if err != nil {
		return fmt.Errorf("check roles: %w", err)
	}

	if roleCount > 0 {
		slog.Debug("Auth data already seeded, skipping")
		return nil
	}

	slog.Info("Seeding authentication data...")

	// Create default roles
	roles := []struct {
		name        string
		description string
		isSystem    bool
	}{
		{"admin", "Full system access, user management", true},
		{"operator", "View all data, manage alerts and drones", true},
		{"viewer", "Read-only access to assigned TAPs", true},
	}

	roleIDs := make(map[string]int)
	for _, r := range roles {
		var id int
		err := s.pg.QueryRow(ctx, `
			INSERT INTO roles (name, description, is_system)
			VALUES ($1, $2, $3)
			ON CONFLICT (name) DO UPDATE SET description = EXCLUDED.description
			RETURNING id
		`, r.name, r.description, r.isSystem).Scan(&id)
		if err != nil {
			return fmt.Errorf("create role %s: %w", r.name, err)
		}
		roleIDs[r.name] = id
	}

	// Create permissions
	permissions := []struct {
		name        string
		description string
		resource    string
		action      string
	}{
		// Dashboard
		{"dashboard.view", "View command center dashboard", "dashboard", "view"},

		// Airspace
		{"airspace.view", "View airspace map", "airspace", "view"},

		// Drones/UAVs
		{"drones.view", "View drone list and details", "drones", "view"},
		{"drones.tag", "Tag/label drones", "drones", "tag"},
		{"drones.hide", "Hide drones from view", "drones", "hide"},
		{"drones.classify", "Change drone classification", "drones", "classify"},

		// Alerts
		{"alerts.view", "View alerts", "alerts", "view"},
		{"alerts.acknowledge", "Acknowledge alerts", "alerts", "acknowledge"},
		{"alerts.dismiss", "Dismiss/clear alerts", "alerts", "dismiss"},

		// Taps/Sensors
		{"taps.view", "View sensor status", "taps", "view"},
		{"taps.configure", "Configure sensors", "taps", "configure"},

		// Analytics
		{"analytics.view", "View analytics dashboard", "analytics", "view"},
		{"analytics.export", "Export analytics data", "analytics", "export"},

		// System
		{"system.view", "View system status", "system", "view"},
		{"system.configure", "Configure system settings", "system", "configure"},

		// Users
		{"users.view", "View user list", "users", "view"},
		{"users.create", "Create new users", "users", "create"},
		{"users.edit", "Edit users", "users", "edit"},
		{"users.delete", "Delete users", "users", "delete"},
		{"users.manage_roles", "Assign roles to users", "users", "manage_roles"},
		{"users.manage_taps", "Assign TAP access to users", "users", "manage_taps"},

		// Data management
		{"data.export", "Export detection data", "data", "export"},
		{"data.clear", "Clear historical data", "data", "clear"},
	}

	permIDs := make(map[string]int)
	for _, p := range permissions {
		var id int
		err := s.pg.QueryRow(ctx, `
			INSERT INTO permissions (name, description, resource, action)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (name) DO UPDATE SET description = EXCLUDED.description
			RETURNING id
		`, p.name, p.description, p.resource, p.action).Scan(&id)
		if err != nil {
			return fmt.Errorf("create permission %s: %w", p.name, err)
		}
		permIDs[p.name] = id
	}

	// Assign permissions to roles
	adminPerms := []string{
		"dashboard.view", "airspace.view",
		"drones.view", "drones.tag", "drones.hide", "drones.classify",
		"alerts.view", "alerts.acknowledge", "alerts.dismiss",
		"taps.view", "taps.configure",
		"analytics.view", "analytics.export",
		"system.view", "system.configure",
		"users.view", "users.create", "users.edit", "users.delete", "users.manage_roles", "users.manage_taps",
		"data.export", "data.clear",
	}

	operatorPerms := []string{
		"dashboard.view", "airspace.view",
		"drones.view", "drones.tag", "drones.hide", "drones.classify",
		"alerts.view", "alerts.acknowledge", "alerts.dismiss",
		"taps.view",
		"analytics.view", "analytics.export",
		"system.view",
		"data.export",
	}

	viewerPerms := []string{
		"dashboard.view", "airspace.view",
		"drones.view",
		"alerts.view",
		"taps.view",
		"analytics.view",
		"system.view",
	}

	rolePerms := map[string][]string{
		"admin":    adminPerms,
		"operator": operatorPerms,
		"viewer":   viewerPerms,
	}

	for roleName, perms := range rolePerms {
		roleID := roleIDs[roleName]
		for _, permName := range perms {
			permID := permIDs[permName]
			_, err := s.pg.Exec(ctx, `
				INSERT INTO role_permissions (role_id, permission_id)
				VALUES ($1, $2)
				ON CONFLICT DO NOTHING
			`, roleID, permID)
			if err != nil {
				return fmt.Errorf("assign permission %s to role %s: %w", permName, roleName, err)
			}
		}
	}

	// Create default admin user with admin:admin credentials
	defaultPassword := "admin"
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(defaultPassword), 12)
	if err != nil {
		return fmt.Errorf("hash default password: %w", err)
	}

	_, err = s.pg.Exec(ctx, `
		INSERT INTO users (username, email, password_hash, display_name, role_id, is_active, is_locked, failed_attempts)
		VALUES ('admin', 'admin@skylens.local', $1, 'Administrator', $2, true, false, 0)
		ON CONFLICT (username) DO UPDATE SET
			password_hash = $1,
			is_active = true,
			is_locked = false,
			failed_attempts = 0,
			locked_until = NULL
	`, string(hashedPassword), roleIDs["admin"])
	if err != nil {
		return fmt.Errorf("create/reset admin user: %w", err)
	}

	slog.Info("Admin user ready - credentials: admin:admin")

	slog.Info("Auth data seeded successfully",
		"roles", len(roles),
		"permissions", len(permissions),
	)

	return nil
}

// GetPG returns the PostgreSQL pool for direct access (used by auth package)
func (s *Store) GetPG() interface{} {
	return s.pg
}

// GetRedis returns the Redis client for direct access (used by auth package)
func (s *Store) GetRedis() interface{} {
	return s.redis
}

// GetContext returns the store's context
func (s *Store) GetContext() context.Context {
	return s.ctx
}
