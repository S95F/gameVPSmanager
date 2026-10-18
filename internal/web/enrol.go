package web

// The enrolment page (Phase 48 item 1): a friend opens the single-use URL
// `gvm user invite` printed, chooses their own password, scans their own TOTP
// secret and proves one code — root never knows either factor. The write
// happens in gvmd (auth.enroll) in one transaction, so a partial enrolment
// cannot exist.

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/S95F/gameVPSmanager/internal/identity"
	"github.com/S95F/gameVPSmanager/internal/webapi"
)

// enrolTTL bounds how long the shown TOTP secret stays redeemable in this
// process; the invite row has its own, longer expiry.
const enrolTTL = 15 * time.Minute

// enrolSecrets holds the TOTP secret shown on the form between GET and POST,
// keyed by the invite token. In-process and single-node, like the pending
// login step: the secret must not be persisted anywhere before the code
// proves the authenticator has it.
type enrolSecrets struct {
	mu sync.Mutex
	m  map[string]enrolSecret
}

type enrolSecret struct {
	Secret  string
	Expires time.Time
}

func newEnrolSecrets() *enrolSecrets { return &enrolSecrets{m: map[string]enrolSecret{}} }

// ForToken returns the secret already shown for this invite, or generates and
// remembers a fresh one. fresh reports a NEW secret (the form says to rescan).
func (s *enrolSecrets) ForToken(token string) (secret string, fresh bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if time.Now().After(v.Expires) {
			delete(s.m, k)
		}
	}
	if v, ok := s.m[token]; ok {
		return v.Secret, false, nil
	}
	secret, err = identity.NewTOTPSecret()
	if err != nil {
		return "", false, err
	}
	s.m[token] = enrolSecret{Secret: secret, Expires: time.Now().Add(enrolTTL)}
	return secret, true, nil
}

// Drop forgets a redeemed invite's secret.
func (s *enrolSecrets) Drop(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, token)
}

func (s *Server) handleEnrolForm(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Identity == nil {
		renderEnrolDead(w, http.StatusNotImplemented,
			"Enrolment is not available: the dashboard is running without its identity backend.")
		return
	}
	token := r.URL.Query().Get("token")
	info, ok := s.enrolLookup(w, r, token)
	if !ok {
		return
	}
	secret, _, err := s.enrol.ForToken(token)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	renderEnrolPage(w, http.StatusOK, token, info.Email, secret, "")
}

func (s *Server) handleEnrolSubmit(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Identity == nil {
		renderEnrolDead(w, http.StatusNotImplemented,
			"Enrolment is not available: the dashboard is running without its identity backend.")
		return
	}
	token := r.PostFormValue("token")
	info, ok := s.enrolLookup(w, r, token)
	if !ok {
		return
	}
	secret, fresh, err := s.enrol.ForToken(token)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if fresh {
		// The process restarted (or the form sat beyond the TTL): the secret
		// the authenticator holds is gone, so a code could never match.
		renderEnrolPage(w, http.StatusConflict, token, info.Email, secret,
			"That took a while — scan this NEW secret and try again.")
		return
	}

	password := r.PostFormValue("password")
	if utf8.RuneCountInString(password) < identity.MinPasswordRunes {
		renderEnrolPage(w, http.StatusUnprocessableEntity, token, info.Email, secret,
			fmt.Sprintf("The password needs at least %d characters.", identity.MinPasswordRunes))
		return
	}
	if password != r.PostFormValue("password2") {
		renderEnrolPage(w, http.StatusUnprocessableEntity, token, info.Email, secret,
			"The passwords do not match.")
		return
	}

	start := time.Now()
	err = s.cfg.Identity.Enroll(r.Context(), token, password, secret, r.PostFormValue("code"))
	holdFloor(start)
	switch {
	case err == nil:
		s.enrol.Drop(token)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case errors.Is(err, ErrTOTPInvalid):
		renderEnrolPage(w, http.StatusUnauthorized, token, info.Email, secret,
			"That code is not valid. Enter the next one from your authenticator.")
	case errors.Is(err, ErrInviteInvalid):
		renderEnrolDead(w, http.StatusGone, enrolDeadText)
	case errors.Is(err, ErrIdentityUnavailable):
		webapi.ServeDegraded(w, r)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// enrolLookup validates the token and renders the dead-invite page itself
// when it cannot; ok=false means the response was written.
func (s *Server) enrolLookup(w http.ResponseWriter, r *http.Request, token string) (InviteInfo, bool) {
	if token == "" {
		renderEnrolDead(w, http.StatusNotFound, enrolDeadText)
		return InviteInfo{}, false
	}
	info, err := s.cfg.Identity.LookupInvite(r.Context(), token)
	switch {
	case err == nil:
		return info, true
	case errors.Is(err, ErrInviteInvalid):
		renderEnrolDead(w, http.StatusGone, enrolDeadText)
	case errors.Is(err, ErrIdentityUnavailable):
		webapi.ServeDegraded(w, r)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
	return InviteInfo{}, false
}

// enrolDeadText never says WHY the invite is dead (unknown, used, expired):
// the URL may have leaked, and the page is anonymous.
const enrolDeadText = "This invite link is no longer valid. Ask the server operator for a fresh one — " +
	"they can mint it with: sudo gvm user reset <your-email> --password --totp"

func renderEnrolDead(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	fmt.Fprint(w, `<title>gvm — enrolment</title>`)
	fmt.Fprint(w, `<link rel="stylesheet" href="/static/gvm.css">`)
	fmt.Fprint(w, `</head><body><main><h1>Enrolment</h1>`)
	fmt.Fprintf(w, `<p class="flash">%s</p>`, htmlEscape(msg))
	fmt.Fprint(w, `</main></body></html>`)
}

func renderEnrolPage(w http.ResponseWriter, status int, token, email, secret, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	fmt.Fprint(w, `<title>gvm — set up your account</title>`)
	fmt.Fprint(w, `<link rel="stylesheet" href="/static/gvm.css">`)
	fmt.Fprint(w, `</head><body><main>`)
	fmt.Fprintf(w, `<h1>Set up your account</h1><p>for <strong>%s</strong></p>`, htmlEscape(email))
	if errMsg != "" {
		fmt.Fprintf(w, `<p class="flash">%s</p>`, htmlEscape(errMsg))
	}
	fmt.Fprint(w, `<form method="post" action="/enrol">`)
	fmt.Fprintf(w, `<input type="hidden" name="token" value="%s">`, htmlEscape(token))
	fmt.Fprintf(w, `<label>Choose a password (min %d characters) <input type="password" name="password" autocomplete="new-password" required></label>`,
		identity.MinPasswordRunes)
	fmt.Fprint(w, `<label>Repeat it <input type="password" name="password2" autocomplete="new-password" required></label>`)
	fmt.Fprint(w, `<p>Add this account to your authenticator app:</p>`)
	fmt.Fprintf(w, `<pre>otpauth://totp/gvm:%s?secret=%s&amp;issuer=gvm</pre>`, htmlEscape(email), htmlEscape(secret))
	fmt.Fprintf(w, `<p>(or enter the secret by hand: <code>%s</code>)</p>`, htmlEscape(secret))
	fmt.Fprint(w, `<label>Enter the 6-digit code it shows <input type="text" name="code" inputmode="numeric" autocomplete="one-time-code" required></label>`)
	fmt.Fprint(w, `<button type="submit">Finish setup</button>`)
	fmt.Fprint(w, `</form></main></body></html>`)
}
