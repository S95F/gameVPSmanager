// Package auth provides HTTP authentication middleware for gvm-web.
//
// Session lookup is delegated to a SessionQuerier (typically backed by
// the store package). The middleware itself only inspects the session cookie
// and sets the Session in the request context; it never talks to Postgres
// directly.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// sessionKey is the unexported context key for the session value.
type sessionKey struct{}

// Session holds the fields extracted from a valid gvm session.
type Session struct {
	ID          string
	UserID      string
	Email       string
	DisplayName string
	Role        string
}

// Claims is the interface that wraps Session access.
// Any value placed in the request context by this package satisfies it.
type Claims interface {
	Session() *Session
}

// FromContext retrieves the Session stored by WithSession.
// The second return value is false when no session is present.
func FromContext(ctx context.Context) (*Session, bool) {
	v, ok := ctx.Value(sessionKey{}).(*Session)
	return v, ok && v != nil
}

// WithSession returns a copy of ctx with sess stored under the session key.
func WithSession(ctx context.Context, sess *Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, sess)
}

// Cookie and bearer constants.
const (
	SessionCookieName = "gvm_sess"
	BearerPrefix      = "Bearer "
)

// Sentinel errors returned by SessionQuerier implementations and tested by
// the middleware to pick the right response code.
var (
	ErrSessionNotFound  = errors.New("auth: session not found")
	ErrSessionExpired   = errors.New("auth: session expired")
	ErrStoreUnavailable = errors.New("auth: store unavailable")
)

// SessionQuerier is the minimal interface the middleware needs.
// The store package satisfies this with a real Postgres query; tests use stubs.
type SessionQuerier interface {
	LookupSession(ctx context.Context, id string) (*Session, error)
}

// Middleware validates session cookies and sets the session in the context.
type Middleware struct {
	querier SessionQuerier
}

// NewMiddleware returns a Middleware backed by querier.
// When querier is nil the middleware always serves the 503 page.
func NewMiddleware(querier SessionQuerier) *Middleware {
	return &Middleware{querier: querier}
}

// Wrap returns an http.Handler that authenticates the request before
// calling next.
//
// Rules:
//   - No cookie → call next with no session in context (handlers decide
//     whether auth is required).
//   - Valid cookie → set session in context; on /api/v1/* without a Bearer
//     header → 401.
//   - Store unavailable → 503 with specific HTML (or JSON for /api/ paths).
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Detect API path early so we can choose the right 503 body.
		isAPI := strings.HasPrefix(r.URL.Path, "/api/")

		cookie, err := r.Cookie(SessionCookieName)
		if err != nil {
			// No session cookie — proceed unauthenticated.
			next.ServeHTTP(w, r)
			return
		}

		if m.querier == nil {
			serve503(w, r, isAPI)
			return
		}

		sess, err := m.querier.LookupSession(r.Context(), cookie.Value)
		if err != nil {
			switch {
			case errors.Is(err, ErrStoreUnavailable):
				serve503(w, r, isAPI)
			case errors.Is(err, ErrSessionNotFound), errors.Is(err, ErrSessionExpired):
				// Clear the stale cookie and send the client to the login page.
				http.SetCookie(w, &http.Cookie{
					Name:   SessionCookieName,
					Value:  "",
					MaxAge: -1,
					Path:   "/",
				})
				if isAPI {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":"session expired"}`))
				} else {
					http.Redirect(w, r, "/login", http.StatusSeeOther)
				}
			default:
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
			return
		}

		ctx := WithSession(r.Context(), sess)
		r = r.WithContext(ctx)

		// D-40: valid session cookie with no bearer token → 401 on every /api/v1 route.
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			if !strings.HasPrefix(r.Header.Get("Authorization"), BearerPrefix) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"bearer token required"}`))
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// serve503 writes the appropriate 503 response based on whether the request
// is for an API path or a browser page.
func serve503(w http.ResponseWriter, r *http.Request, isAPI bool) {
	if isAPI {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"datastore unavailable"}`))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(downPage503))
}

// downPage503 is the specific 503 page required by the architecture doc.
const downPage503 = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>gvm dashboard — datastore down</title>
<style>
body{font-family:monospace;max-width:72ch;margin:4rem auto;padding:0 1rem;background:#1a1a1a;color:#e0e0e0}
h1{color:#f08080}pre{background:#111;padding:1rem;border-radius:4px;overflow-x:auto}
</style>
</head>
<body>
<h1>gvm dashboard unavailable — the datastore is down.</h1>
<p>Your game servers are <strong>NOT</strong> affected. They run independently of the dashboard.</p>
<p>To investigate the Postgres issue:</p>
<pre>  systemctl status postgresql
  journalctl -u postgresql -n 50
  df -h /var/lib/postgresql</pre>
<p>Everything the dashboard does is available from the box:</p>
<pre>  sudo gvm status      sudo gvm console pz-main
  sudo gvm stop pz-main   sudo gvm doctor</pre>
</body>
</html>`

// ----------------------------------------------------------------------------
// CSRF protection
// ----------------------------------------------------------------------------

// CSRFProtection generates and validates CSRF tokens using HMAC-SHA256.
// Tokens are bound to the session by storing the session ID in the HMAC data.
type CSRFProtection struct {
	secret []byte
}

// NewCSRF returns a CSRFProtection keyed by secret.
// secret must be at least 32 bytes; callers should derive it from the
// application's master secret via HKDF.
func NewCSRF(secret []byte) *CSRFProtection {
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &CSRFProtection{secret: cp}
}

// Token returns the CSRF token for the current session.
// When no session is in the context it returns an ephemeral random token so
// that forms on non-authenticated pages still carry a valid token.
func (c *CSRFProtection) Token(ctx context.Context) string {
	var sid string
	if sess, ok := FromContext(ctx); ok {
		sid = sess.ID
	}
	return c.tokenFor(sid)
}

// tokenFor generates the HMAC token for a session ID.
func (c *CSRFProtection) tokenFor(sid string) string {
	// Mix a per-request random nonce with the session to prevent collision
	// across sessions. The nonce is embedded in the token so Validate can
	// reproduce the HMAC without storing state.
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		// Fallback: use session ID bytes — still better than nothing.
		nonce = []byte(sid)
		if len(nonce) > 16 {
			nonce = nonce[:16]
		}
	}
	mac := hmac.New(sha256.New, c.secret)
	mac.Write(nonce)
	mac.Write([]byte(sid))
	sum := mac.Sum(nil)
	// token = nonce_hex + "." + hmac_hex
	return hex.EncodeToString(nonce) + "." + hex.EncodeToString(sum)
}

// Validate checks the X-CSRF-Token header or _csrf form field.
// It returns a non-nil error when the token is missing or invalid.
func (c *CSRFProtection) Validate(r *http.Request) error {
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		// Fall back to form field.
		token = r.FormValue("_csrf")
	}
	if token == "" {
		return errors.New("csrf: missing token")
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return errors.New("csrf: malformed token")
	}
	nonce, err := hex.DecodeString(parts[0])
	if err != nil {
		return errors.New("csrf: malformed token nonce")
	}
	gotMAC, err := hex.DecodeString(parts[1])
	if err != nil {
		return errors.New("csrf: malformed token mac")
	}

	var sid string
	if sess, ok := FromContext(r.Context()); ok {
		sid = sess.ID
	}

	mac := hmac.New(sha256.New, c.secret)
	mac.Write(nonce)
	mac.Write([]byte(sid))
	wantMAC := mac.Sum(nil)

	if !hmac.Equal(gotMAC, wantMAC) {
		return errors.New("csrf: token mismatch")
	}
	return nil
}

// sessionClaims implements Claims for a *Session value placed in context.
// A compile-time check that it satisfies the interface.
type sessionClaims struct{ s *Session }

func (sc sessionClaims) Session() *Session { return sc.s }

var _ Claims = sessionClaims{}
