// Package webq contains hand-authored query functions for the gvm_web role.
// These simulate what sqlc would generate for the column-restricted web tier.
//
// gvm_web cannot SELECT: totp_secret_ct, totp_key_id, last_totp_step (D-27).
// It has INSERT+SELECT on audit_log (append-only by grant, D-23).
// It can SELECT most other tables but cannot UPDATE or DELETE append-only ones.
package webq

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// UserRow is the subset of the users table that gvm_web may read.
// The TOTP secret fields are excluded (D-27).
type UserRow struct {
	ID                 string
	Email              string
	DisplayName        string
	PasswordHash       *string
	MustChangePassword bool
	GlobalRole         string
	TOTPEnrolledAt     *time.Time
	FailedLogins       int
	LockedUntil        *time.Time
	LastLoginAt        *time.Time
	DisabledAt         *time.Time
	CreatedAt          time.Time
}

// GetUserByEmail returns the user row for email, using only the columns that
// gvm_web is allowed to read.
func GetUserByEmail(ctx context.Context, pool *pgxpool.Pool, email string) (UserRow, error) {
	var u UserRow
	err := pool.QueryRow(ctx, `
		SELECT id, email, display_name, password_hash, must_change_password,
		       global_role, totp_enrolled_at, failed_logins, locked_until,
		       last_login_at, disabled_at, created_at
		  FROM users
		 WHERE email = $1`, email).Scan(
		&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &u.MustChangePassword,
		&u.GlobalRole, &u.TOTPEnrolledAt, &u.FailedLogins, &u.LockedUntil,
		&u.LastLoginAt, &u.DisabledAt, &u.CreatedAt,
	)
	if err != nil {
		return UserRow{}, fmt.Errorf("webq: get user by email: %w", err)
	}
	return u, nil
}

// SessionRow represents a row in the sessions table.
type SessionRow struct {
	ID         string
	UserID     string
	IP         *string
	UserAgent  *string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	RevokedAt  *time.Time
}

// GetSession returns the session with the given id, or an error if not found.
func GetSession(ctx context.Context, pool *pgxpool.Pool, id string) (SessionRow, error) {
	var s SessionRow
	err := pool.QueryRow(ctx, `
		SELECT id, user_id, ip::text, user_agent, created_at, expires_at, last_seen_at, revoked_at
		  FROM sessions
		 WHERE id = $1`, id).Scan(
		&s.ID, &s.UserID, &s.IP, &s.UserAgent,
		&s.CreatedAt, &s.ExpiresAt, &s.LastSeenAt, &s.RevokedAt,
	)
	if err != nil {
		return SessionRow{}, fmt.Errorf("webq: get session: %w", err)
	}
	return s, nil
}

// CreateSession inserts a new session row.
func CreateSession(ctx context.Context, pool *pgxpool.Pool, s SessionRow) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, ip, user_agent, created_at, expires_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		s.ID, s.UserID, s.IP, s.UserAgent, s.CreatedAt, s.ExpiresAt, s.LastSeenAt,
	)
	if err != nil {
		return fmt.Errorf("webq: create session: %w", err)
	}
	return nil
}

// RevokeSession sets revoked_at on a session.
func RevokeSession(ctx context.Context, pool *pgxpool.Pool, id string) error {
	_, err := pool.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("webq: revoke session: %w", err)
	}
	return nil
}

// TouchSession updates last_seen_at.
func TouchSession(ctx context.Context, pool *pgxpool.Pool, id string) error {
	_, err := pool.Exec(ctx, `UPDATE sessions SET last_seen_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("webq: touch session: %w", err)
	}
	return nil
}
