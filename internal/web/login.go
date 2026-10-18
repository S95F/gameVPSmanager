package web

// The login engine (web-dashboard-3, admin-security-4): password, then TOTP,
// then — and only then — a session. The password is verified here against
// gvm_web's readable hash; the TOTP code is verified in gvmd, because the
// secret's columns are unreadable to the web tier by grant (D-27, rule 9).
//
// With no Identity wired (this binary run before Phase 48's wiring, or a
// test), the POST is an honest 501: a form that cannot sign anyone in must
// say so, not 500 or silently re-render (U3).

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/S95F/gameVPSmanager/internal/auth"
	"github.com/S95F/gameVPSmanager/internal/webapi"
)

// loginUnavailableMessage is the one honest sentence for an unwired login
// engine. Dashboard accounts are created with `gvm user create` on the host.
const loginUnavailableMessage = "Signing in is not available: the dashboard is running without its " +
	"identity backend. On the host, use the gvm CLI over SSH."

// loginFloor is the fixed response-time floor on credential checks, so a fast
// rejection cannot be timed against a slow argon2 verify (§16.2). A package
// variable so tests can zero it.
var loginFloor = 250 * time.Millisecond

// sessionLifetime is the session cookie's MaxAge (§16.2: 12 h).
const sessionLifetime = 12 * time.Hour

// holdFloor sleeps whatever remains of the floor.
func holdFloor(start time.Time) {
	if rest := loginFloor - time.Since(start); rest > 0 {
		time.Sleep(rest)
	}
}

// clientIP is the peer address without the port; behind the unix socket it is
// whatever the listener reports, recorded verbatim.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// bootstrapIfEmpty serves the empty-users page when no account exists: no
// login form, just the exact command that creates the first owner (item 1).
// It reports whether it handled the request.
func (s *Server) bootstrapIfEmpty(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Identity == nil {
		return false
	}
	n, err := s.cfg.Identity.CountUsers(r.Context())
	if err != nil {
		webapi.ServeDegraded(w, r)
		return true
	}
	if n > 0 {
		return false
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	fmt.Fprint(w, `<meta name="viewport" content="width=device-width, initial-scale=1">`)
	fmt.Fprint(w, `<title>gvm — set up the first account</title>`)
	fmt.Fprint(w, `<link rel="stylesheet" href="/static/gvm.css">`)
	fmt.Fprint(w, `</head><body><main><h1>No accounts yet</h1>`)
	fmt.Fprint(w, `<p>This dashboard has no users. On the host, as root, create the first owner:</p>`)
	fmt.Fprint(w, `<pre>sudo gvm user create --email you@example.com --role owner</pre>`)
	fmt.Fprint(w, `<p>Then reload this page to sign in.</p>`)
	fmt.Fprint(w, `</main></body></html>`)
	return true
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if s.bootstrapIfEmpty(w, r) {
		return
	}
	renderLoginPage(w, http.StatusOK, r.URL.Query().Get("next"), "")
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	next := r.PostFormValue("next")
	if s.cfg.Identity == nil {
		renderLoginPage(w, http.StatusNotImplemented, next, loginUnavailableMessage)
		return
	}
	if s.bootstrapIfEmpty(w, r) {
		return
	}

	start := time.Now()
	email := r.PostFormValue("email")
	password := r.PostFormValue("password")
	user, err := s.cfg.Identity.VerifyPassword(r.Context(), email, password, clientIP(r))
	holdFloor(start)

	switch {
	case err == nil:
		// fall through
	case errors.Is(err, ErrBadCredentials):
		renderLoginPage(w, http.StatusUnauthorized, next, "Wrong email or password.")
		return
	case errors.Is(err, ErrAccountLocked), errors.Is(err, ErrRateLimited):
		w.Header().Set("Retry-After", "900")
		renderLoginPage(w, http.StatusTooManyRequests, next,
			"Too many attempts. Wait a few minutes and try again.")
		return
	case errors.Is(err, ErrIdentityUnavailable):
		webapi.ServeDegraded(w, r)
		return
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if !user.TOTPEnrolled {
		// No second factor, no session — ever (rule 9). The fix is the
		// operator's, over SSH.
		renderLoginPage(w, http.StatusForbidden, next,
			"This account has no authenticator enrolled. The server operator can reset it: "+
				"sudo gvm user reset "+user.Email+" --totp")
		return
	}

	id := s.pending.Put(pendingLogin{User: user, Next: safeNext(next)})
	http.SetCookie(w, &http.Cookie{
		Name: pendingCookie, Value: id, Path: "/login",
		MaxAge: int(pendingTTL.Seconds()), HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: !s.cfg.InsecureCookies,
	})
	http.Redirect(w, r, "/login/totp", http.StatusSeeOther)
}

// pendingFrom resolves the pending-login cookie, if a live one is present.
func (s *Server) pendingFrom(r *http.Request) (string, pendingLogin, bool) {
	c, err := r.Cookie(pendingCookie)
	if err != nil {
		return "", pendingLogin{}, false
	}
	p, ok := s.pending.Get(c.Value)
	return c.Value, p, ok
}

func (s *Server) handleTOTPForm(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Identity != nil {
		if _, _, ok := s.pendingFrom(r); !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
	}
	renderTOTPPage(w, http.StatusOK, "")
}

func (s *Server) handleTOTPSubmit(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Identity == nil {
		renderTOTPPage(w, http.StatusNotImplemented, loginUnavailableMessage)
		return
	}
	id, p, ok := s.pendingFrom(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	start := time.Now()
	err := s.cfg.Identity.VerifyTOTP(r.Context(), p.User.Email, r.PostFormValue("code"))
	holdFloor(start)

	switch {
	case err == nil:
		// fall through
	case errors.Is(err, ErrTOTPInvalid):
		renderTOTPPage(w, http.StatusUnauthorized, "That code is not valid. Enter the next one from your authenticator.")
		return
	case errors.Is(err, ErrNoTOTP):
		renderTOTPPage(w, http.StatusForbidden, "This account has no authenticator enrolled; the server operator can reset it over SSH.")
		return
	case errors.Is(err, ErrIdentityUnavailable):
		webapi.ServeDegraded(w, r)
		return
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if _, ok := s.pending.Take(id); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	sid, err := s.cfg.Identity.CreateSession(r.Context(), p.User.ID, clientIP(r), r.UserAgent())
	if err != nil {
		webapi.ServeDegraded(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: pendingCookie, Value: "", Path: "/login", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookieName, Value: sid, Path: "/",
		MaxAge: int(sessionLifetime.Seconds()), HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: !s.cfg.InsecureCookies,
	})
	http.Redirect(w, r, p.Next, http.StatusSeeOther)
}

// handleLogout revokes the session row and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Identity != nil {
		if c, err := r.Cookie(auth.SessionCookieName); err == nil {
			// Best-effort: a failed revoke still clears the cookie, and the
			// row expires on its own.
			_ = s.cfg.Identity.RevokeSession(r.Context(), c.Value)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:   auth.SessionCookieName,
		Value:  "",
		MaxAge: -1,
		Path:   "/",
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func renderLoginPage(w http.ResponseWriter, status int, next, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	fmt.Fprint(w, `<meta name="viewport" content="width=device-width, initial-scale=1">`)
	fmt.Fprint(w, `<title>gvm — sign in</title>`)
	fmt.Fprint(w, `<link rel="stylesheet" href="/static/gvm.css">`)
	fmt.Fprint(w, `</head><body><main><h1>Sign in</h1>`)
	if errMsg != "" {
		fmt.Fprintf(w, `<p class="flash">%s</p>`, htmlEscape(errMsg))
	}
	fmt.Fprint(w, `<form method="post" action="/login">`)
	fmt.Fprintf(w, `<input type="hidden" name="next" value="%s">`, htmlEscape(next))
	fmt.Fprint(w, `<label>Email <input type="email" name="email" autocomplete="username" required></label>`)
	fmt.Fprint(w, `<label>Password <input type="password" name="password" autocomplete="current-password" required></label>`)
	fmt.Fprint(w, `<button type="submit">Sign in</button>`)
	fmt.Fprint(w, `</form></main></body></html>`)
}

func renderTOTPPage(w http.ResponseWriter, status int, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	fmt.Fprint(w, `<meta name="viewport" content="width=device-width, initial-scale=1">`)
	fmt.Fprint(w, `<title>gvm — verification code</title>`)
	fmt.Fprint(w, `<link rel="stylesheet" href="/static/gvm.css">`)
	fmt.Fprint(w, `</head><body><main><h1>Verification code</h1>`)
	if errMsg != "" {
		fmt.Fprintf(w, `<p class="flash">%s</p>`, htmlEscape(errMsg))
	}
	fmt.Fprint(w, `<form method="post" action="/login/totp">`)
	fmt.Fprint(w, `<label>Code <input type="text" name="code" inputmode="numeric" autocomplete="one-time-code" required></label>`)
	fmt.Fprint(w, `<button type="submit">Verify</button>`)
	fmt.Fprint(w, `</form></main></body></html>`)
}
