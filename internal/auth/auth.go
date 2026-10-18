// Package auth provides the two HTTP authentication paths for gvm-web, kept
// structurally apart per D-40:
//
//   - Sessions (this file): cookie → Session in the request context, loaded by
//     LoadSession and enforced by RequireAuth. Only the browser mux uses it.
//   - Bearer tokens (bearer.go): Authorization header → TokenActor. Only the
//     /api/v1 mux uses it, and it never reads a cookie.
//
// Session lookup is delegated to a SessionQuerier (typically backed by the
// store package). The middleware itself only inspects the session cookie and
// sets the Session in the request context; it never talks to Postgres
// directly.
package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/S95F/gameVPSmanager/internal/webapi"
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

// SessionCookieName is the browser session cookie. (The __Host- prefix and
// Secure attribute arrive with the login engine in Phase 48; the middleware
// only reads the cookie, so the name is all it needs.)
const SessionCookieName = "gvm_sess"

// Sentinel errors returned by SessionQuerier implementations and tested by
// the middleware to pick the right response.
var (
	ErrSessionNotFound  = errors.New("auth: session not found")
	ErrSessionExpired   = errors.New("auth: session expired")
	ErrStoreUnavailable = errors.New("auth: store unavailable")
)

// SessionQuerier is the minimal interface the session middleware needs.
// The store package satisfies this with a real Postgres query; tests use stubs.
type SessionQuerier interface {
	LookupSession(ctx context.Context, id string) (*Session, error)
}

// Middleware resolves session cookies for the browser mux.
type Middleware struct {
	querier SessionQuerier
}

// NewMiddleware returns a Middleware backed by querier.
// When querier is nil every request stays anonymous (RequireAuth then sends
// the browser to /login), except that a present cookie with no store to check
// it against serves the degraded page rather than silently downgrading a
// maybe-valid session to anonymous.
func NewMiddleware(querier SessionQuerier) *Middleware {
	return &Middleware{querier: querier}
}

// LoadSession resolves the session cookie, if any, into the request context.
// It never *requires* a session — that is RequireAuth's job — so the login
// page and static assets flow through it unchanged.
//
//   - no cookie → next, anonymous
//   - stale/unknown cookie → clear it, next, anonymous
//   - store unavailable → the single 503 template (webapi.ServeDegraded)
func (m *Middleware) LoadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(SessionCookieName)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		if m.querier == nil {
			webapi.ServeDegraded(w, r)
			return
		}

		sess, err := m.querier.LookupSession(r.Context(), cookie.Value)
		if err != nil {
			switch {
			case errors.Is(err, ErrStoreUnavailable):
				webapi.ServeDegraded(w, r)
			case errors.Is(err, ErrSessionNotFound), errors.Is(err, ErrSessionExpired):
				// Clear the stale cookie and continue anonymously; RequireAuth
				// will redirect to /login with the right ?next=.
				http.SetCookie(w, &http.Cookie{
					Name:   SessionCookieName,
					Value:  "",
					MaxAge: -1,
					Path:   "/",
				})
				next.ServeHTTP(w, r)
			default:
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
			return
		}

		next.ServeHTTP(w, r.WithContext(WithSession(r.Context(), sess)))
	})
}

// RequireAuth refuses anonymous requests on a browser route (web-dashboard-3).
//
//   - plain browser → 303 to /login?next=<original URL>
//   - htmx request (HX-Request: true) → 401 plus HX-Redirect, because htmx
//     would otherwise swap the login page into a fragment (§16.3)
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		target := "/login?next=" + url.QueryEscape(r.URL.RequestURI())
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", target)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	})
}
