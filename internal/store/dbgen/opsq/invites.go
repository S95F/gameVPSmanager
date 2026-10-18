package opsq

// Enrolment invites (Phase 48 item 1). The root CLI mints them; gvmd redeems
// them — the one write path for a self-enrolled password and TOTP secret.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInviteInvalid covers every way a presented invite can be dead — unknown,
// used, expired, its user disabled — because the enrolment page shows one
// sentence for all of them.
var ErrInviteInvalid = errors.New("opsq: invite is not valid")

// CreateInvite mints an invite row for the user with the given email.
func CreateInvite(ctx context.Context, pool *pgxpool.Pool, id, email string, tokenSHA256 []byte, ttl time.Duration) error {
	tag, err := pool.Exec(ctx, `
		INSERT INTO user_invites (id, user_id, token_sha256, expires_at)
		SELECT $1, u.id, $3, now() + $4::interval FROM users u WHERE u.email = $2`,
		id, email, tokenSHA256, fmt.Sprintf("%d seconds", int(ttl.Seconds())))
	if err != nil {
		return fmt.Errorf("opsq: create invite: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no user %q", ErrNotFound, email)
	}
	return nil
}

// RevokeInvites invalidates every unredeemed invite for email (a reset mints
// a fresh one; the old URL must die with the old factors).
func RevokeInvites(ctx context.Context, pool *pgxpool.Pool, email string) error {
	_, err := pool.Exec(ctx, `
		UPDATE user_invites i SET used_at = now()
		  FROM users u
		 WHERE i.user_id = u.id AND u.email = $1 AND i.used_at IS NULL`, email)
	if err != nil {
		return fmt.Errorf("opsq: revoke invites: %w", err)
	}
	return nil
}

// ConsumeInviteEnroll redeems an invite in one transaction: it locks the
// invite row, refuses a used/expired/disabled one, writes the enrolment
// (password hash, sealed TOTP secret) and marks the invite used. Partial
// enrolments cannot exist.
func ConsumeInviteEnroll(ctx context.Context, pool *pgxpool.Pool, tokenSHA256 []byte, passwordHash string, sealedTOTP []byte, keyID int16) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("opsq: enroll: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var inviteID, userID string
	var expiresAt time.Time
	var usedAt *time.Time
	var disabled bool
	err = tx.QueryRow(ctx, `
		SELECT i.id, i.user_id, i.expires_at, i.used_at, u.disabled_at IS NOT NULL
		  FROM user_invites i JOIN users u ON u.id = i.user_id
		 WHERE i.token_sha256 = $1
		 FOR UPDATE OF i`, tokenSHA256).Scan(&inviteID, &userID, &expiresAt, &usedAt, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInviteInvalid
	}
	if err != nil {
		return fmt.Errorf("opsq: enroll: %w", err)
	}
	if usedAt != nil || disabled || time.Now().After(expiresAt) {
		return ErrInviteInvalid
	}

	if _, err := tx.Exec(ctx, `
		UPDATE users SET password_hash = $2, must_change_password = false,
		       totp_secret_ct = $3, totp_key_id = $4, totp_enrolled_at = now(),
		       last_totp_step = NULL, failed_logins = 0, locked_until = NULL
		 WHERE id = $1`, userID, passwordHash, sealedTOTP, keyID); err != nil {
		return fmt.Errorf("opsq: enroll: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE user_invites SET used_at = now() WHERE id = $1`, inviteID); err != nil {
		return fmt.Errorf("opsq: enroll: %w", err)
	}
	return tx.Commit(ctx)
}

// GetInviteUserID resolves an invite's user for sealing, without consuming it.
func GetInviteUserID(ctx context.Context, pool *pgxpool.Pool, tokenSHA256 []byte) (string, error) {
	var userID string
	var expiresAt time.Time
	var usedAt *time.Time
	err := pool.QueryRow(ctx, `
		SELECT user_id, expires_at, used_at FROM user_invites WHERE token_sha256 = $1`,
		tokenSHA256).Scan(&userID, &expiresAt, &usedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInviteInvalid
	}
	if err != nil {
		return "", fmt.Errorf("opsq: get invite: %w", err)
	}
	if usedAt != nil || time.Now().After(expiresAt) {
		return "", ErrInviteInvalid
	}
	return userID, nil
}
