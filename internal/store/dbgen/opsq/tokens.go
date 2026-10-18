package opsq

// API-token queries for the ROOT CLI (`gvm token`). The plaintext never
// reaches this layer: callers store the SHA-256 and the display prefix.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAmbiguous is returned when a revoke needle matches more than one token.
var ErrAmbiguous = errors.New("opsq: matches more than one token")

// CreateAPIToken inserts a token for the user with the given email.
func CreateAPIToken(ctx context.Context, pool *pgxpool.Pool, id, email, name, prefix string, sha256 []byte, roleCeiling string, expiresAt *time.Time) error {
	tag, err := pool.Exec(ctx, `
		INSERT INTO api_tokens (id, user_id, name, prefix, token_sha256, role_ceiling, expires_at)
		SELECT $1, u.id, $3, $4, $5, $6::role_t, $7 FROM users u WHERE u.email = $2`,
		id, email, name, prefix, sha256, roleCeiling, expiresAt)
	if err != nil {
		return fmt.Errorf("opsq: create api token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no user %q", ErrNotFound, email)
	}
	return nil
}

// TokenListRow is one line of `gvm token list`.
type TokenListRow struct {
	ID          string
	Email       string
	Name        string
	Prefix      string
	RoleCeiling string
	CreatedAt   time.Time
	ExpiresAt   *time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
}

// ListAPITokens returns every token, newest first.
func ListAPITokens(ctx context.Context, pool *pgxpool.Pool) ([]TokenListRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT t.id, u.email, t.name, t.prefix, t.role_ceiling,
		       t.created_at, t.expires_at, t.last_used_at, t.revoked_at
		  FROM api_tokens t JOIN users u ON u.id = t.user_id
		 ORDER BY t.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("opsq: list api tokens: %w", err)
	}
	defer rows.Close()
	var out []TokenListRow
	for rows.Next() {
		var t TokenListRow
		if err := rows.Scan(&t.ID, &t.Email, &t.Name, &t.Prefix, &t.RoleCeiling,
			&t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, fmt.Errorf("opsq: list api tokens: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAPIToken revokes the one live token whose name or prefix equals
// needle. An ambiguous needle is refused rather than guessed at.
func RevokeAPIToken(ctx context.Context, pool *pgxpool.Pool, needle string) (TokenListRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT t.id, u.email, t.name, t.prefix, t.role_ceiling,
		       t.created_at, t.expires_at, t.last_used_at, t.revoked_at
		  FROM api_tokens t JOIN users u ON u.id = t.user_id
		 WHERE t.revoked_at IS NULL AND (t.name = $1 OR t.prefix = $1)`, needle)
	if err != nil {
		return TokenListRow{}, fmt.Errorf("opsq: revoke api token: %w", err)
	}
	defer rows.Close()
	var matches []TokenListRow
	for rows.Next() {
		var t TokenListRow
		if err := rows.Scan(&t.ID, &t.Email, &t.Name, &t.Prefix, &t.RoleCeiling,
			&t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return TokenListRow{}, fmt.Errorf("opsq: revoke api token: %w", err)
		}
		matches = append(matches, t)
	}
	if err := rows.Err(); err != nil {
		return TokenListRow{}, fmt.Errorf("opsq: revoke api token: %w", err)
	}
	switch len(matches) {
	case 0:
		return TokenListRow{}, fmt.Errorf("%w: no live token named or prefixed %q", ErrNotFound, needle)
	case 1:
	default:
		return TokenListRow{}, fmt.Errorf("%w: %q names %d live tokens; revoke by prefix", ErrAmbiguous, needle, len(matches))
	}
	if _, err := pool.Exec(ctx, `UPDATE api_tokens SET revoked_at = now() WHERE id = $1`, matches[0].ID); err != nil {
		return TokenListRow{}, fmt.Errorf("opsq: revoke api token: %w", err)
	}
	return matches[0], nil
}
