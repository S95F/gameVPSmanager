package webq

// Login, token and authorization queries for the WEB tier, within gvm_web's
// column grants (migration 0010): it can read a user's password hash and
// lockout fields, never the TOTP secret (D-27) — TOTP is verified in gvmd.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("webq: not found")

// GetUserByID returns the user row by id, same column set as GetUserByEmail.
func GetUserByID(ctx context.Context, pool *pgxpool.Pool, id string) (UserRow, error) {
	var u UserRow
	err := pool.QueryRow(ctx, `
		SELECT id, email, display_name, password_hash, must_change_password,
		       global_role, totp_enrolled_at, failed_logins, locked_until,
		       last_login_at, disabled_at, created_at
		  FROM users
		 WHERE id = $1`, id).Scan(
		&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &u.MustChangePassword,
		&u.GlobalRole, &u.TOTPEnrolledAt, &u.FailedLogins, &u.LockedUntil,
		&u.LastLoginAt, &u.DisabledAt, &u.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserRow{}, ErrNotFound
	}
	if err != nil {
		return UserRow{}, fmt.Errorf("webq: get user by id: %w", err)
	}
	return u, nil
}

// CountUsers backs the empty-users bootstrap page.
func CountUsers(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("webq: count users: %w", err)
	}
	return n, nil
}

// RecordLoginAttempt appends one login_attempts row (§16.2's rate limiting
// reads these; the row is also the operator's forensic record).
func RecordLoginAttempt(ctx context.Context, pool *pgxpool.Pool, email, ip string, success bool, reason string) error {
	var ipArg *string
	if ip != "" {
		ipArg = &ip
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO login_attempts (email, ip, success, reason)
		VALUES ($1, $2, $3, $4)`, email, ipArg, success, reason)
	if err != nil {
		return fmt.Errorf("webq: record login attempt: %w", err)
	}
	return nil
}

// BumpFailedLogins increments the per-account failure counter and locks the
// account for lockFor once failures reach lockAfter. It returns the new count
// and the lock, if one is now set.
func BumpFailedLogins(ctx context.Context, pool *pgxpool.Pool, userID string, lockAfter int, lockFor time.Duration) (int, *time.Time, error) {
	var failed int
	var lockedUntil *time.Time
	err := pool.QueryRow(ctx, `
		UPDATE users
		   SET failed_logins = failed_logins + 1,
		       locked_until = CASE WHEN failed_logins + 1 >= $2
		                           THEN now() + $3::interval
		                           ELSE locked_until END
		 WHERE id = $1
		 RETURNING failed_logins, locked_until`,
		userID, lockAfter, fmt.Sprintf("%d seconds", int(lockFor.Seconds()))).
		Scan(&failed, &lockedUntil)
	if err != nil {
		return 0, nil, fmt.Errorf("webq: bump failed logins: %w", err)
	}
	return failed, lockedUntil, nil
}

// ClearFailedLogins resets the failure counter and stamps a successful login.
func ClearFailedLogins(ctx context.Context, pool *pgxpool.Pool, userID string) error {
	_, err := pool.Exec(ctx, `
		UPDATE users SET failed_logins = 0, locked_until = NULL, last_login_at = now()
		 WHERE id = $1`, userID)
	if err != nil {
		return fmt.Errorf("webq: clear failed logins: %w", err)
	}
	return nil
}

// CountRecentIPFailures counts failed attempts from one address inside the
// window (§16.2: 10 failures / 15 min per IP).
func CountRecentIPFailures(ctx context.Context, pool *pgxpool.Pool, ip string, window time.Duration) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `
		SELECT count(*) FROM login_attempts
		 WHERE success = false AND ip = $1::inet
		   AND at > now() - $2::interval`,
		ip, fmt.Sprintf("%d seconds", int(window.Seconds()))).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("webq: count ip failures: %w", err)
	}
	return n, nil
}

// ── API tokens ───────────────────────────────────────────────────────────────

// APITokenRow is the bearer-auth lookup result: the token joined with the two
// user fields auth needs.
type APITokenRow struct {
	ID           string
	UserID       string
	Name         string
	RoleCeiling  string
	UserRole     string
	UserDisabled bool
	ExpiresAt    *time.Time
	RevokedAt    *time.Time
}

// GetAPITokenBySHA resolves the SHA-256 of a presented bearer token.
func GetAPITokenBySHA(ctx context.Context, pool *pgxpool.Pool, sha []byte) (APITokenRow, error) {
	var t APITokenRow
	err := pool.QueryRow(ctx, `
		SELECT t.id, t.user_id, t.name, t.role_ceiling, u.global_role,
		       u.disabled_at IS NOT NULL, t.expires_at, t.revoked_at
		  FROM api_tokens t JOIN users u ON u.id = t.user_id
		 WHERE t.token_sha256 = $1`, sha).Scan(
		&t.ID, &t.UserID, &t.Name, &t.RoleCeiling, &t.UserRole,
		&t.UserDisabled, &t.ExpiresAt, &t.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return APITokenRow{}, ErrNotFound
	}
	if err != nil {
		return APITokenRow{}, fmt.Errorf("webq: get api token: %w", err)
	}
	return t, nil
}

// TouchAPIToken stamps last_used_at / last_used_ip (within gvm_web's column
// UPDATE grant). Best-effort at the caller.
func TouchAPIToken(ctx context.Context, pool *pgxpool.Pool, id string, at time.Time, ip string) error {
	var ipArg *string
	if ip != "" {
		ipArg = &ip
	}
	_, err := pool.Exec(ctx, `
		UPDATE api_tokens SET last_used_at = $2, last_used_ip = $3 WHERE id = $1`,
		id, at, ipArg)
	if err != nil {
		return fmt.Errorf("webq: touch api token: %w", err)
	}
	return nil
}

// ── Authorization ────────────────────────────────────────────────────────────

// EffectiveRole returns the role a user holds for an instance: the grant when
// one exists, the global role otherwise. Grants only ever ADD (§15.2), so the
// greater of the two is taken in SQL.
func EffectiveRole(ctx context.Context, pool *pgxpool.Pool, userID, instanceName string) (string, error) {
	var role string
	err := pool.QueryRow(ctx, `
		WITH ranked AS (
			SELECT unnest(enum_range(NULL::role_t))::text AS name,
			       generate_series(1, array_length(enum_range(NULL::role_t), 1)) AS rank
		)
		SELECT COALESCE(
		    (SELECT r.name FROM ranked r
		      WHERE r.name IN (
		            (SELECT u.global_role::text FROM users u WHERE u.id = $1),
		            (SELECT ig.role::text
		               FROM instance_grants ig JOIN instances i ON i.id = ig.instance_id
		              WHERE ig.user_id = $1 AND i.name = $2)
		      )
		      ORDER BY r.rank DESC LIMIT 1),
		    (SELECT u.global_role::text FROM users u WHERE u.id = $1)
		)`, userID, instanceName).Scan(&role)
	if err != nil {
		return "", fmt.Errorf("webq: effective role: %w", err)
	}
	return role, nil
}
