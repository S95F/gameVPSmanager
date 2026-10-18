package web

// The login engine, driven with a stub Identity: password → TOTP → session,
// with every refusal honest and no session until both factors pass (rule 9).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/authz"
)

func init() {
	// The 250 ms credential floor guards production timing, not tests.
	loginFloor = 0
}

type stubIdentity struct {
	users     int
	password  string
	code      string
	enrolled  bool
	locked    bool
	sessions  []string
	revoked   []string
	createErr error
}

func (s *stubIdentity) CountUsers(context.Context) (int, error) { return s.users, nil }

func (s *stubIdentity) VerifyPassword(_ context.Context, email, password, ip string) (UserInfo, error) {
	if s.locked {
		return UserInfo{}, ErrAccountLocked
	}
	if email != "alice@example.com" || password != s.password {
		return UserInfo{}, ErrBadCredentials
	}
	return UserInfo{ID: "u1", Email: email, Role: authz.RoleOperator, TOTPEnrolled: s.enrolled}, nil
}

func (s *stubIdentity) VerifyTOTP(_ context.Context, email, code string) error {
	if !s.enrolled {
		return ErrNoTOTP
	}
	if code != s.code {
		return ErrTOTPInvalid
	}
	return nil
}

func (s *stubIdentity) CreateSession(_ context.Context, userID, ip, ua string) (string, error) {
	if s.createErr != nil {
		return "", s.createErr
	}
	s.sessions = append(s.sessions, userID)
	return "s1", nil // the id the recordingSessions stub accepts
}

func (s *stubIdentity) RevokeSession(_ context.Context, id string) error {
	s.revoked = append(s.revoked, id)
	return nil
}

// invite is the one token the stub accepts; enrolments record their inputs.
func (s *stubIdentity) LookupInvite(_ context.Context, token string) (InviteInfo, error) {
	if token == "gvmi_good" {
		return InviteInfo{Email: "friend@example.com"}, nil
	}
	return InviteInfo{}, ErrInviteInvalid
}

func (s *stubIdentity) Enroll(_ context.Context, token, password, totpSecret, code string) error {
	if token != "gvmi_good" {
		return ErrInviteInvalid
	}
	if code != s.code {
		return ErrTOTPInvalid
	}
	s.enrolled = true
	s.sessions = append(s.sessions, "enrolled:"+password)
	return nil
}

func loginServer(t *testing.T, id *stubIdentity) http.Handler {
	t.Helper()
	s, _ := newTestServer(t, func(c *Config) {
		c.Identity = id
		c.InsecureCookies = true
	})
	return s.Handler()
}

// postForm sends a form POST carrying any cookies from previous responses.
func postForm(h http.Handler, target string, form map[string]string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	vals := make([]string, 0, len(form))
	for k, v := range form {
		vals = append(vals, k+"="+v)
	}
	req := httptest.NewRequest("POST", target, strings.NewReader(strings.Join(vals, "&")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestEmptyUsersBootstrapPage: with zero accounts the login page is setup
// instructions carrying the exact create command — and no form (item 1).
func TestEmptyUsersBootstrapPage(t *testing.T) {
	h := loginServer(t, &stubIdentity{users: 0})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "sudo gvm user create --email you@example.com --role owner") {
		t.Error("bootstrap page must carry the exact first-owner command")
	}
	if strings.Contains(body, "<form") {
		t.Error("bootstrap page must have no login form")
	}
}

// TestLoginHappyPath walks password → TOTP → session → authenticated page.
func TestLoginHappyPath(t *testing.T) {
	id := &stubIdentity{users: 1, password: "correct horse battery staple", code: "123456", enrolled: true}
	h := loginServer(t, id)

	rec := postForm(h, "/login", map[string]string{
		"email": "alice@example.com", "password": "correct horse battery staple",
		"next": "/i/pz-main",
	}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login/totp" {
		t.Fatalf("password step: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(id.sessions) != 0 {
		t.Fatal("a session existed before the second factor (rule 9)")
	}

	rec = postForm(h, "/login/totp", map[string]string{"code": "123456"}, cookies)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("totp step: %d; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/i/pz-main" {
		t.Errorf("post-login redirect = %q, want the carried next", loc)
	}
	if len(id.sessions) != 1 {
		t.Fatal("no session was created after both factors")
	}
	var sessCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "gvm_sess" && c.Value != "" {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("no session cookie set")
	}

	// The issued session opens an authenticated page.
	req := httptest.NewRequest("GET", "/instances", nil)
	req.AddCookie(sessCookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("authenticated page after login = %d", rec.Code)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	id := &stubIdentity{users: 1, password: "right", enrolled: true}
	h := loginServer(t, id)
	rec := postForm(h, "/login", map[string]string{"email": "alice@example.com", "password": "wrong"}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Wrong email or password") {
		t.Error("the refusal should be the one generic sentence")
	}
}

func TestLoginLockedIs429(t *testing.T) {
	id := &stubIdentity{users: 1, locked: true}
	h := loginServer(t, id)
	rec := postForm(h, "/login", map[string]string{"email": "alice@example.com", "password": "x"}, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a lockout should carry Retry-After")
	}
}

// TestLoginWithoutTOTPIsRefused: no second factor, no session — the fix is
// the operator's, over SSH (rule 9).
func TestLoginWithoutTOTPIsRefused(t *testing.T) {
	id := &stubIdentity{users: 1, password: "right", enrolled: false}
	h := loginServer(t, id)
	rec := postForm(h, "/login", map[string]string{"email": "alice@example.com", "password": "right"}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "sudo gvm user reset") {
		t.Error("the refusal should hand the operator the reset command")
	}
	if len(id.sessions) != 0 {
		t.Error("a session was created with one factor")
	}
}

// TestWrongCodeKeepsThePendingLogin: a mistyped code re-renders and the next,
// correct code still signs in (the pending step is consumed on success only).
func TestWrongCodeKeepsThePendingLogin(t *testing.T) {
	id := &stubIdentity{users: 1, password: "right", code: "654321", enrolled: true}
	h := loginServer(t, id)

	rec := postForm(h, "/login", map[string]string{"email": "alice@example.com", "password": "right"}, nil)
	cookies := rec.Result().Cookies()

	rec = postForm(h, "/login/totp", map[string]string{"code": "000000"}, cookies)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d, want 401", rec.Code)
	}
	rec = postForm(h, "/login/totp", map[string]string{"code": "654321"}, cookies)
	if rec.Code != http.StatusSeeOther || len(id.sessions) != 1 {
		t.Errorf("right code after a wrong one: %d, sessions=%v", rec.Code, id.sessions)
	}
}

// TestTOTPPageWithoutPendingRedirects: visiting the code page cold goes back
// to /login; there is nothing to verify against.
func TestTOTPPageWithoutPendingRedirects(t *testing.T) {
	h := loginServer(t, &stubIdentity{users: 1})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login/totp", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("cold totp page: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
}

// TestNextIsKeptOnSite: a scheme-relative or absolute next cannot bounce the
// browser off this site after login.
func TestNextIsKeptOnSite(t *testing.T) {
	id := &stubIdentity{users: 1, password: "right", code: "1", enrolled: true}
	h := loginServer(t, id)

	rec := postForm(h, "/login", map[string]string{
		"email": "alice@example.com", "password": "right", "next": "//evil.example/phish",
	}, nil)
	cookies := rec.Result().Cookies()
	rec = postForm(h, "/login/totp", map[string]string{"code": "1"}, cookies)
	if loc := rec.Header().Get("Location"); loc != "/instances" {
		t.Errorf("open redirect: %q", loc)
	}
}

// TestLogoutRevokesTheSession: logout revokes the row, not just the cookie.
func TestLogoutRevokesTheSession(t *testing.T) {
	id := &stubIdentity{users: 1}
	h := loginServer(t, id)

	req := httptest.NewRequest("POST", "/logout", nil)
	req.AddCookie(&http.Cookie{Name: "gvm_sess", Value: "s1"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d", rec.Code)
	}
	if len(id.revoked) != 1 || id.revoked[0] != "s1" {
		t.Errorf("revoked = %v, want the session row", id.revoked)
	}
}
