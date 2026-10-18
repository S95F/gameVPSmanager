package opsq

// User, grant and session queries for the ROOT CLI (`gvm user`), which talks
// to Postgres as gvm_ops over peer auth. Nothing here is reachable from the
// web tier: gvm_web's column grants (migration 0010) exclude the TOTP secret,
// and the web tier's own queries live in webq.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a named user, grant or token does not exist.
var ErrNotFound = errors.New("opsq: not found")

// CreateUser inserts a new account. passwordHash may be empty for an account
// that will enrol through an invite later; role is the global role_t value.
func CreateUser(ctx context.Context, pool *pgxpool.Pool, id, email, displayName, passwordHash, role string) error {
	var hash *string
	if passwordHash != "" {
		hash = &passwordHash
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, display_name, password_hash, global_role)
		VALUES ($1, $2, $3, $4, $5)`,
		id, email, displayName, hash, role)
	if err != nil {
		return fmt.Errorf("opsq: create user: %w", err)
	}
	return nil
}

// CountUsers returns the number of accounts, disabled ones included — the
// empty-users bootstrap page must disappear the moment the first account
// exists, even a disabled one.
func CountUsers(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("opsq: count users: %w", err)
	}
	return n, nil
}

// GetUserID resolves an email to the account id.
func GetUserID(ctx context.Context, pool *pgxpool.Pool, email string) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: no user %q", ErrNotFound, email)
	}
	if err != nil {
		return "", fmt.Errorf("opsq: get user id: %w", err)
	}
	return id, nil
}

// UserListRow is one line of `gvm user list`.
type UserListRow struct {
	ID           string
	Email        string
	DisplayName  string
	GlobalRole   string
	TOTPEnrolled bool
	LastLoginAt  *time.Time
	DisabledAt   *time.Time
	CreatedAt    time.Time
}

// ListUsers returns every account, oldest first.
func ListUsers(ctx context.Context, pool *pgxpool.Pool) ([]UserListRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, email, display_name, global_role,
		       totp_enrolled_at IS NOT NULL, last_login_at, disabled_at, created_at
		  FROM users ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("opsq: list users: %w", err)
	}
	defer rows.Close()
	var out []UserListRow
	for rows.Next() {
		var u UserListRow
		if err := rows.Scan(&u.ID, &u.Email, &u.DisplayName, &u.GlobalRole,
			&u.TOTPEnrolled, &u.LastLoginAt, &u.DisabledAt, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("opsq: list users: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUserDisabled disables (or re-enables) an account. Disabling also revokes
// every live session and API token in the same transaction, because a
// disabled account that keeps a working session is not disabled.
func SetUserDisabled(ctx context.Context, pool *pgxpool.Pool, email string, disabled bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("opsq: disable user: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	set := `disabled_at = CASE WHEN $2 THEN now() ELSE NULL END`
	err = tx.QueryRow(ctx, `UPDATE users SET `+set+` WHERE email = $1 RETURNING id`, email, disabled).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no user %q", ErrNotFound, email)
	}
	if err != nil {
		return fmt.Errorf("opsq: disable user: %w", err)
	}
	if disabled {
		if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, id); err != nil {
			return fmt.Errorf("opsq: disable user sessions: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE api_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, id); err != nil {
			return fmt.Errorf("opsq: disable user tokens: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// SetTOTP stores a user's sealed TOTP secret and stamps enrolment.
func SetTOTP(ctx context.Context, pool *pgxpool.Pool, userID string, sealed []byte, keyID int16) error {
	tag, err := pool.Exec(ctx, `
		UPDATE users SET totp_secret_ct = $2, totp_key_id = $3,
		       totp_enrolled_at = now(), last_totp_step = NULL
		 WHERE id = $1`, userID, sealed, keyID)
	if err != nil {
		return fmt.Errorf("opsq: set totp: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no user id %q", ErrNotFound, userID)
	}
	return nil
}

// TOTPRow is what gvmd's auth.verify_totp verb reads. Only gvm_ops can read
// these columns (D-27).
type TOTPRow struct {
	UserID   string
	Sealed   []byte
	KeyID    int16
	LastStep int64
	Enrolled bool
	Disabled bool
}

// GetTOTP loads the sealed TOTP secret for an email.
func GetTOTP(ctx context.Context, pool *pgxpool.Pool, email string) (TOTPRow, error) {
	var r TOTPRow
	var lastStep *int64
	err := pool.QueryRow(ctx, `
		SELECT id, COALESCE(totp_secret_ct, ''::bytea), totp_key_id,
		       last_totp_step, totp_enrolled_at IS NOT NULL, disabled_at IS NOT NULL
		  FROM users WHERE email = $1`, email).Scan(
		&r.UserID, &r.Sealed, &r.KeyID, &lastStep, &r.Enrolled, &r.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return TOTPRow{}, fmt.Errorf("%w: no user %q", ErrNotFound, email)
	}
	if err != nil {
		return TOTPRow{}, fmt.Errorf("opsq: get totp: %w", err)
	}
	if lastStep != nil {
		r.LastStep = *lastStep
	}
	return r, nil
}

// SetLastTOTPStep records the accepted step so a code can never be replayed.
func SetLastTOTPStep(ctx context.Context, pool *pgxpool.Pool, userID string, step int64) error {
	_, err := pool.Exec(ctx, `UPDATE users SET last_totp_step = $2 WHERE id = $1`, userID, step)
	if err != nil {
		return fmt.Errorf("opsq: set last totp step: %w", err)
	}
	return nil
}

// ResetUserFactors clears the chosen factors and revokes every session and
// token, so root never chooses a friend's password — the account re-enrols.
func ResetUserFactors(ctx context.Context, pool *pgxpool.Pool, email string, password, totp bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("opsq: reset user: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no user %q", ErrNotFound, email)
	}
	if err != nil {
		return fmt.Errorf("opsq: reset user: %w", err)
	}
	if password {
		if _, err := tx.Exec(ctx, `
			UPDATE users SET password_hash = NULL, must_change_password = true,
			       failed_logins = 0, locked_until = NULL WHERE id = $1`, id); err != nil {
			return fmt.Errorf("opsq: reset password: %w", err)
		}
	}
	if totp {
		if _, err := tx.Exec(ctx, `
			UPDATE users SET totp_secret_ct = NULL, totp_enrolled_at = NULL,
			       last_totp_step = NULL WHERE id = $1`, id); err != nil {
			return fmt.Errorf("opsq: reset totp: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, id); err != nil {
		return fmt.Errorf("opsq: reset sessions: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE api_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, id); err != nil {
		return fmt.Errorf("opsq: reset tokens: %w", err)
	}
	return tx.Commit(ctx)
}

// ── Grants ───────────────────────────────────────────────────────────────────

// UpsertInstance makes sure the named instance has a mirror row, so a grant
// can reference it before gvmd's first sweep. The filesystem stays the control
// plane: this writes only the mirror.
func UpsertInstance(ctx context.Context, pool *pgxpool.Pool, id, name, game, yamlPath string) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO instances (id, name, game, yaml_path)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE
		   SET game = EXCLUDED.game, yaml_path = EXCLUDED.yaml_path,
		       absent_since = NULL, updated_at = now()`,
		id, name, game, yamlPath)
	if err != nil {
		return fmt.Errorf("opsq: upsert instance: %w", err)
	}
	return nil
}

// SetGrant gives email the role on the named instance (upsert).
func SetGrant(ctx context.Context, pool *pgxpool.Pool, email, instanceName, role string) error {
	tag, err := pool.Exec(ctx, `
		INSERT INTO instance_grants (user_id, instance_id, role)
		SELECT u.id, i.id, $3::role_t FROM users u, instances i
		 WHERE u.email = $1 AND i.name = $2
		ON CONFLICT (user_id, instance_id) DO UPDATE SET role = EXCLUDED.role, granted_at = now()`,
		email, instanceName, role)
	if err != nil {
		return fmt.Errorf("opsq: set grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: user %q or instance %q", ErrNotFound, email, instanceName)
	}
	return nil
}

// RevokeGrant removes email's grant on the named instance.
func RevokeGrant(ctx context.Context, pool *pgxpool.Pool, email, instanceName string) error {
	tag, err := pool.Exec(ctx, `
		DELETE FROM instance_grants ig
		 USING users u, instances i
		 WHERE ig.user_id = u.id AND ig.instance_id = i.id
		   AND u.email = $1 AND i.name = $2`, email, instanceName)
	if err != nil {
		return fmt.Errorf("opsq: revoke grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no grant for %q on %q", ErrNotFound, email, instanceName)
	}
	return nil
}

// GrantRow is one instance grant.
type GrantRow struct {
	Instance  string
	Role      string
	GrantedAt time.Time
}

// ListGrants returns email's instance grants.
func ListGrants(ctx context.Context, pool *pgxpool.Pool, email string) ([]GrantRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT i.name, ig.role, ig.granted_at
		  FROM instance_grants ig
		  JOIN users u ON u.id = ig.user_id
		  JOIN instances i ON i.id = ig.instance_id
		 WHERE u.email = $1 ORDER BY i.name`, email)
	if err != nil {
		return nil, fmt.Errorf("opsq: list grants: %w", err)
	}
	defer rows.Close()
	var out []GrantRow
	for rows.Next() {
		var g GrantRow
		if err := rows.Scan(&g.Instance, &g.Role, &g.GrantedAt); err != nil {
			return nil, fmt.Errorf("opsq: list grants: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ── Sessions ─────────────────────────────────────────────────────────────────

// SessionListRow is one line of `gvm user sessions`.
type SessionListRow struct {
	ID         string
	Email      string
	IP         *string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// ListLiveSessions returns the unexpired, unrevoked sessions, optionally for
// one email ("" = all users).
func ListLiveSessions(ctx context.Context, pool *pgxpool.Pool, email string) ([]SessionListRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT s.id, u.email, s.ip::text, s.created_at, s.last_seen_at, s.expires_at
		  FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.revoked_at IS NULL AND s.expires_at > now()
		   AND ($1 = '' OR u.email = $1)
		 ORDER BY s.last_seen_at DESC`, email)
	if err != nil {
		return nil, fmt.Errorf("opsq: list sessions: %w", err)
	}
	defer rows.Close()
	var out []SessionListRow
	for rows.Next() {
		var s SessionListRow
		if err := rows.Scan(&s.ID, &s.Email, &s.IP, &s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt); err != nil {
			return nil, fmt.Errorf("opsq: list sessions: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeUserSessions revokes every live session for email and returns how many.
func RevokeUserSessions(ctx context.Context, pool *pgxpool.Pool, email string) (int64, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE sessions s SET revoked_at = now()
		  FROM users u
		 WHERE s.user_id = u.id AND u.email = $1 AND s.revoked_at IS NULL`, email)
	if err != nil {
		return 0, fmt.Errorf("opsq: revoke sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}
