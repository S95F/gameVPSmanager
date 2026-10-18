// Package webstore binds the web tier's interfaces to their backends:
// Postgres as gvm_web (sessions, users, tokens, attempts — through webq, so
// every query stays inside that role's column grants) and the gvmd gateway
// (TOTP, status, lifecycle — the things the web tier must not do itself).
//
// internal/web stays interface-driven and test-stubbed; this package is the
// one place those interfaces meet pgx and the admin socket, wired by
// `gvm web` at startup.
package webstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/S95F/gameVPSmanager/internal/auth"
	"github.com/S95F/gameVPSmanager/internal/authz"
	"github.com/S95F/gameVPSmanager/internal/gvmd"
	"github.com/S95F/gameVPSmanager/internal/identity"
	"github.com/S95F/gameVPSmanager/internal/store"
	"github.com/S95F/gameVPSmanager/internal/store/dbgen/webq"
	"github.com/S95F/gameVPSmanager/internal/web"
)

// Lockout policy (§16.2): five failures per account, fifteen minutes; ten
// failures per address in fifteen minutes.
const (
	lockAfterFailures = 5
	lockFor           = 15 * time.Minute
	ipWindow          = 15 * time.Minute
	ipMaxFailures     = 10
)

// Store implements auth.SessionQuerier, auth.TokenQuerier, web.Identity and
// web.Authorizer over one connection pool and one gvmd client.
type Store struct {
	st   *store.Store
	gvmd *gvmd.Client
}

// New binds st and client. The pool may still be probing; every method
// answers "unavailable" rather than blocking, so the dashboard serves its
// degraded page instead of hanging (rule 1).
func New(st *store.Store, client *gvmd.Client) *Store {
	return &Store{st: st, gvmd: client}
}

// pool returns the live pool or errUnavailable without waiting.
func (s *Store) pool() (*pgxpool.Pool, bool) {
	select {
	case <-s.st.Ready():
		return s.st.Pool(), true
	default:
		return nil, false
	}
}

// ── auth.SessionQuerier ──────────────────────────────────────────────────────

// LookupSession resolves a cookie to a live session and its user.
func (s *Store) LookupSession(ctx context.Context, id string) (*auth.Session, error) {
	pool, ok := s.pool()
	if !ok {
		return nil, auth.ErrStoreUnavailable
	}
	row, err := webq.GetSession(ctx, pool, id)
	if err != nil {
		if errors.Is(err, webq.ErrNotFound) {
			return nil, auth.ErrSessionNotFound
		}
		return nil, fmt.Errorf("webstore: %w: %v", auth.ErrStoreUnavailable, err)
	}
	now := time.Now()
	if row.RevokedAt != nil || now.After(row.ExpiresAt) {
		return nil, auth.ErrSessionExpired
	}
	user, err := webq.GetUserByID(ctx, pool, row.UserID)
	if err != nil {
		if errors.Is(err, webq.ErrNotFound) {
			return nil, auth.ErrSessionNotFound
		}
		return nil, fmt.Errorf("webstore: %w: %v", auth.ErrStoreUnavailable, err)
	}
	if user.DisabledAt != nil {
		// A disabled account's session is dead, whatever its expiry says.
		return nil, auth.ErrSessionExpired
	}
	// Best-effort idle stamp; a failed touch must not fail the request.
	_ = webq.TouchSession(ctx, pool, id)
	return &auth.Session{
		ID:          row.ID,
		UserID:      user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		Role:        user.GlobalRole,
	}, nil
}

// ── auth.TokenQuerier ────────────────────────────────────────────────────────

// LookupAPIToken resolves a presented bearer token's hash.
func (s *Store) LookupAPIToken(ctx context.Context, sha []byte) (*auth.APIToken, error) {
	pool, ok := s.pool()
	if !ok {
		return nil, auth.ErrStoreUnavailable
	}
	row, err := webq.GetAPITokenBySHA(ctx, pool, sha)
	if err != nil {
		if errors.Is(err, webq.ErrNotFound) {
			return nil, auth.ErrTokenNotFound
		}
		return nil, fmt.Errorf("webstore: %w: %v", auth.ErrStoreUnavailable, err)
	}
	return &auth.APIToken{
		ID:           row.ID,
		UserID:       row.UserID,
		Name:         row.Name,
		RoleCeiling:  authz.Role(row.RoleCeiling),
		UserRole:     authz.Role(row.UserRole),
		UserDisabled: row.UserDisabled,
		ExpiresAt:    row.ExpiresAt,
		RevokedAt:    row.RevokedAt,
	}, nil
}

// RecordTokenUse stamps last_used_at; best-effort by contract.
func (s *Store) RecordTokenUse(ctx context.Context, tokenID string, at time.Time, remoteAddr string) error {
	pool, ok := s.pool()
	if !ok {
		return auth.ErrStoreUnavailable
	}
	return webq.TouchAPIToken(ctx, pool, tokenID, at, remoteAddr)
}

// ── web.Identity ─────────────────────────────────────────────────────────────

// CountUsers backs the empty-users bootstrap page.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	pool, ok := s.pool()
	if !ok {
		return 0, web.ErrIdentityUnavailable
	}
	n, err := webq.CountUsers(ctx, pool)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", web.ErrIdentityUnavailable, err)
	}
	return n, nil
}

// VerifyPassword checks email+password with lockout and per-IP limits, and
// records the attempt either way.
func (s *Store) VerifyPassword(ctx context.Context, email, password, ip string) (web.UserInfo, error) {
	pool, ok := s.pool()
	if !ok {
		return web.UserInfo{}, web.ErrIdentityUnavailable
	}

	record := func(success bool, reason string) {
		_ = webq.RecordLoginAttempt(ctx, pool, email, ip, success, reason)
	}

	if ip != "" {
		if n, err := webq.CountRecentIPFailures(ctx, pool, ip, ipWindow); err == nil && n >= ipMaxFailures {
			record(false, "ip rate limited")
			return web.UserInfo{}, web.ErrRateLimited
		}
	}

	user, err := webq.GetUserByEmail(ctx, pool, email)
	if err != nil {
		// Unknown email reads exactly like a wrong password to the visitor;
		// the attempt row keeps the distinction for the operator.
		record(false, "unknown email")
		return web.UserInfo{}, web.ErrBadCredentials
	}
	switch {
	case user.DisabledAt != nil:
		record(false, "account disabled")
		return web.UserInfo{}, web.ErrBadCredentials
	case user.LockedUntil != nil && time.Now().Before(*user.LockedUntil):
		record(false, "account locked")
		return web.UserInfo{}, web.ErrAccountLocked
	case user.PasswordHash == nil:
		record(false, "no password set")
		return web.UserInfo{}, web.ErrBadCredentials
	}

	if err := identity.CheckPassword(*user.PasswordHash, password); err != nil {
		failed, _, bumpErr := webq.BumpFailedLogins(ctx, pool, user.ID, lockAfterFailures, lockFor)
		record(false, "wrong password")
		if bumpErr == nil && failed >= lockAfterFailures {
			return web.UserInfo{}, web.ErrAccountLocked
		}
		return web.UserInfo{}, web.ErrBadCredentials
	}

	if err := webq.ClearFailedLogins(ctx, pool, user.ID); err != nil {
		return web.UserInfo{}, fmt.Errorf("%w: %v", web.ErrIdentityUnavailable, err)
	}
	record(true, "password ok")
	return web.UserInfo{
		ID:           user.ID,
		Email:        user.Email,
		DisplayName:  user.DisplayName,
		Role:         authz.Role(user.GlobalRole),
		TOTPEnrolled: user.TOTPEnrolledAt != nil,
	}, nil
}

// VerifyTOTP asks gvmd (rule 9: the secret never reaches the web tier).
func (s *Store) VerifyTOTP(ctx context.Context, email, code string) error {
	err := s.gvmd.VerifyTOTP(ctx, email, code)
	if err == nil {
		return nil
	}
	var remote *gvmd.RemoteError
	switch {
	case errors.As(err, &remote) && remote.Code == gvmd.CodeTOTPInvalid:
		return web.ErrTOTPInvalid
	case errors.As(err, &remote) && remote.Code == gvmd.CodeNoTOTP:
		return web.ErrNoTOTP
	case errors.As(err, &remote) && remote.Code == gvmd.CodeDegraded:
		return web.ErrIdentityUnavailable
	case errors.Is(err, gvmd.ErrGatewayDown):
		return web.ErrIdentityUnavailable
	default:
		return err
	}
}

// CreateSession issues a session row (§16.2: 12 h lifetime).
func (s *Store) CreateSession(ctx context.Context, userID, ip, userAgent string) (string, error) {
	pool, ok := s.pool()
	if !ok {
		return "", web.ErrIdentityUnavailable
	}
	id, err := identity.NewSessionID()
	if err != nil {
		return "", err
	}
	now := time.Now()
	var ipPtr, uaPtr *string
	if ip != "" {
		ipPtr = &ip
	}
	if userAgent != "" {
		uaPtr = &userAgent
	}
	err = webq.CreateSession(ctx, pool, webq.SessionRow{
		ID: id, UserID: userID, IP: ipPtr, UserAgent: uaPtr,
		CreatedAt: now, ExpiresAt: now.Add(12 * time.Hour), LastSeenAt: now,
	})
	if err != nil {
		return "", fmt.Errorf("%w: %v", web.ErrIdentityUnavailable, err)
	}
	return id, nil
}

// RevokeSession revokes one session (logout).
func (s *Store) RevokeSession(ctx context.Context, sessionID string) error {
	pool, ok := s.pool()
	if !ok {
		return web.ErrIdentityUnavailable
	}
	return webq.RevokeSession(ctx, pool, sessionID)
}

// ── web.Authorizer ───────────────────────────────────────────────────────────

// EffectiveRole resolves the §15.2 effective role for one instance.
func (s *Store) EffectiveRole(ctx context.Context, userID, instance string) (authz.Role, error) {
	pool, ok := s.pool()
	if !ok {
		return "", web.ErrIdentityUnavailable
	}
	role, err := webq.EffectiveRole(ctx, pool, userID, instance)
	if err != nil {
		return "", err
	}
	return authz.ParseRole(role)
}
