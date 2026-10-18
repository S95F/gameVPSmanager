package web

// The login pages (web-dashboard-3). The routes exist so every redirect in
// the auth middleware lands somewhere real, and so Phase 48 can wire the
// credential engine into handlers whose shape — and tests — already hold.
// Until then the POST is an honest 501: a form that cannot sign anyone in
// must say so, not 500 or silently re-render (U3).

import (
	"fmt"
	"net/http"

	"github.com/S95F/gameVPSmanager/internal/auth"
)

// loginUnavailableMessage is the one honest sentence for the unbuilt login
// engine. Dashboard accounts arrive with `gvm user create` in Phase 48.
const loginUnavailableMessage = "Signing in is not in this release; dashboard accounts arrive " +
	"with user management. On the host, use the gvm CLI over SSH."

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	renderLoginPage(w, http.StatusOK, r.URL.Query().Get("next"), "")
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	// No credential verifier exists in this build; nothing checks a password,
	// so nothing may issue a session.
	renderLoginPage(w, http.StatusNotImplemented, r.PostFormValue("next"), loginUnavailableMessage)
}

func (s *Server) handleTOTPForm(w http.ResponseWriter, r *http.Request) {
	renderTOTPPage(w, http.StatusOK, "")
}

func (s *Server) handleTOTPSubmit(w http.ResponseWriter, r *http.Request) {
	renderTOTPPage(w, http.StatusNotImplemented, loginUnavailableMessage)
}

// handleLogout clears the session cookie and lands on the login page. The
// server-side session row is revoked by the login engine in Phase 48; the
// cookie clear is what this build can honestly do.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
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
