package web

// The identity seams the login engine drives. The implementations live in
// internal/webstore (Postgres as gvm_web) and internal/gvmd (TOTP, which the
// web tier cannot verify itself: the secret's columns are unreadable to
// gvm_web by grant, D-27). web stays interface-driven so every flow is tested
// with stubs and no daemon.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/S95F/gameVPSmanager/internal/authz"
)

// UserInfo is what a successful password verification returns.
type UserInfo struct {
	ID           string
	Email        string
	DisplayName  string
	Role         authz.Role
	TOTPEnrolled bool
}

// The login engine's failure modes. One rendered sentence covers wrong email
// and wrong password alike; the distinctions below exist for status codes and
// logging, not for the visitor.
var (
	ErrBadCredentials = errors.New("web: wrong email or password")
	ErrAccountLocked  = errors.New("web: account locked")
	ErrRateLimited    = errors.New("web: too many attempts")
	ErrNoTOTP         = errors.New("web: no authenticator enrolled")
	ErrTOTPInvalid    = errors.New("web: the code is not valid")
	// ErrIdentityUnavailable is Postgres or the gvmd gateway being down; the
	// visitor sees the single degraded template.
	ErrIdentityUnavailable = errors.New("web: identity backend unavailable")
)

// Identity is the login engine's backend.
type Identity interface {
	// CountUsers backs the empty-users bootstrap page (U2): zero accounts
	// means the dashboard shows setup instructions, not a login form.
	CountUsers(ctx context.Context) (int, error)
	// VerifyPassword checks email+password, enforcing lockout and rate
	// limits, and records the attempt. ip is the client address for the
	// attempt row.
	VerifyPassword(ctx context.Context, email, password, ip string) (UserInfo, error)
	// VerifyTOTP checks a code — through gvmd, never locally (rule 9).
	VerifyTOTP(ctx context.Context, email, code string) error
	// CreateSession issues a session row and returns its id.
	CreateSession(ctx context.Context, userID, ip, userAgent string) (string, error)
	// RevokeSession revokes one session (logout).
	RevokeSession(ctx context.Context, sessionID string) error
}

// Authorizer resolves a user's effective role for one instance: the greater
// of the global role and the instance grant (§15.2 — grants only ever add).
type Authorizer interface {
	EffectiveRole(ctx context.Context, userID, instance string) (authz.Role, error)
}

// ── The pending-TOTP step ────────────────────────────────────────────────────
//
// Between a correct password and a correct code the browser holds a pending
// id: an in-process, single-node map (the same scope the dashboard itself
// has), never a session — a session must not exist until both factors passed.

// pendingTTL bounds how long the second factor may take.
const pendingTTL = 5 * time.Minute

// pendingCookie carries the pending id between /login and /login/totp.
const pendingCookie = "gvm_pending"

type pendingLogin struct {
	User    UserInfo
	Next    string
	Expires time.Time
}

type pendingStore struct {
	mu sync.Mutex
	m  map[string]pendingLogin
}

func newPendingStore() *pendingStore {
	return &pendingStore{m: map[string]pendingLogin{}}
}

// Put stores p and returns its one-time id.
func (s *pendingStore) Put(p pendingLogin) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	p.Expires = time.Now().Add(pendingTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Opportunistic sweep so an abandoned login cannot grow the map.
	for k, v := range s.m {
		if time.Now().After(v.Expires) {
			delete(s.m, k)
		}
	}
	s.m[id] = p
	return id
}

// Get returns the pending login without consuming it (a wrong code gets
// another try until the TTL runs out).
func (s *pendingStore) Get(id string) (pendingLogin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[id]
	if !ok || time.Now().After(p.Expires) {
		delete(s.m, id)
		return pendingLogin{}, false
	}
	return p, true
}

// Take consumes the pending login once the second factor passed.
func (s *pendingStore) Take(id string) (pendingLogin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[id]
	delete(s.m, id)
	if !ok || time.Now().After(p.Expires) {
		return pendingLogin{}, false
	}
	return p, true
}

// safeNext keeps the post-login redirect on this site: an absolute path,
// never scheme-relative ("//evil.example") and never a full URL.
func safeNext(next string) string {
	if len(next) < 1 || next[0] != '/' {
		return "/instances"
	}
	if len(next) > 1 && next[1] == '/' {
		return "/instances"
	}
	return next
}
