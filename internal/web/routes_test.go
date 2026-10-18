package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/auth"
	"github.com/S95F/gameVPSmanager/internal/authz"
	"github.com/S95F/gameVPSmanager/internal/identity"
)

// ── Shared stubs ─────────────────────────────────────────────────────────────

// testToken is the plaintext bearer token the stub querier accepts (operator
// ceiling); testViewerToken is its viewer-ceiling sibling for RBAC tests.
const (
	testToken       = "gvmt_test-token"
	testViewerToken = "gvmt_viewer-token"
)

type recordingSessions struct {
	lookups int
}

func (s *recordingSessions) LookupSession(_ context.Context, id string) (*auth.Session, error) {
	s.lookups++
	switch id {
	case "s1":
		return &auth.Session{ID: "s1", UserID: "u1", Email: "op@example.com", Role: "operator"}, nil
	case "s2":
		return &auth.Session{ID: "s2", UserID: "u2", Email: "viewer@example.com", Role: "viewer"}, nil
	}
	return nil, auth.ErrSessionNotFound
}

type stubTokens struct{}

func (stubTokens) LookupAPIToken(_ context.Context, sha []byte) (*auth.APIToken, error) {
	switch string(sha) {
	case string(identity.HashToken(testToken)):
		return &auth.APIToken{
			ID: "tok1", UserID: "u1", Name: "ci",
			RoleCeiling: authz.RoleOperator, UserRole: authz.RoleOperator,
		}, nil
	case string(identity.HashToken(testViewerToken)):
		return &auth.APIToken{
			ID: "tok2", UserID: "u2", Name: "watcher",
			RoleCeiling: authz.RoleViewer, UserRole: authz.RoleOperator,
		}, nil
	}
	return nil, auth.ErrTokenNotFound
}

func (stubTokens) RecordTokenUse(context.Context, string, time.Time, string) error { return nil }

type stubInstances struct{}

func (stubInstances) ListInstances(context.Context) ([]InstanceSummary, error) {
	return []InstanceSummary{{Name: "pz-main", Status: "running", Game: "projectzomboid"}}, nil
}

func (stubInstances) GetInstance(_ context.Context, name string) (*InstanceDetail, error) {
	if name == "pz-main" {
		return &InstanceDetail{Name: "pz-main", Status: "running", Game: "projectzomboid"}, nil
	}
	return nil, errors.New("web: no such instance")
}

type recordingDispatcher struct {
	calls []string
	// err, when set, is what every dispatch returns (e.g. *LeaseHeldError).
	err error
}

func (d *recordingDispatcher) dispatch(verb, name, actor string) (string, error) {
	if d.err != nil {
		return "", d.err
	}
	d.calls = append(d.calls, verb+" "+name+" by "+actor)
	return "op_test", nil
}

func (d *recordingDispatcher) Start(_ context.Context, name, actor string) (string, error) {
	return d.dispatch("start", name, actor)
}

func (d *recordingDispatcher) Stop(_ context.Context, name, actor string) (string, error) {
	return d.dispatch("stop", name, actor)
}

func (d *recordingDispatcher) Restart(_ context.Context, name, actor string) (string, error) {
	return d.dispatch("restart", name, actor)
}

// newTestServer builds a Server on the standard stubs; mutate overrides cfg.
func newTestServer(t *testing.T, mutate func(*Config)) (*Server, *recordingSessions) {
	t.Helper()
	sessions := &recordingSessions{}
	cfg := Config{
		Addr:            "127.0.0.1:0",
		SessionQuerier:  sessions,
		TokenQuerier:    stubTokens{},
		InstanceQuerier: stubInstances{},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, sessions
}

// patternURL turns a route pattern into a concrete request target.
func patternURL(pattern string) string {
	url := strings.ReplaceAll(pattern, "{name}", "pz-main")
	url = strings.ReplaceAll(url, "{file}", "gvm.css")
	url = strings.ReplaceAll(url, "/{$}", "/")
	return url
}

func withSession(req *http.Request) *http.Request {
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "s1"})
	return req
}

// ── web-dashboard-3: every browser route needs a session ─────────────────────

// TestBrowserRoutesRequireSession walks the browser route table: every
// non-public route, hit anonymously, must land on /login?next= and leak
// nothing.
func TestBrowserRoutesRequireSession(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.Handler()

	for _, rt := range browserRoutes {
		if rt.public {
			continue
		}
		t.Run(rt.method+" "+rt.pattern, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(rt.method, patternURL(rt.pattern), nil))
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("anonymous status = %d, want 303 to the login page", rec.Code)
			}
			if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login?next=") {
				t.Errorf("Location = %q, want /login?next=…", loc)
			}
		})
	}
}

// TestBrowserRoutesRequireSessionHtmx: an htmx request must get 401 plus
// HX-Redirect, never a login page swapped into a fragment (§16.3).
func TestBrowserRoutesRequireSessionHtmx(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.Handler()

	req := httptest.NewRequest("GET", "/instances", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("htmx anonymous status = %d, want 401", rec.Code)
	}
	if hx := rec.Header().Get("HX-Redirect"); !strings.HasPrefix(hx, "/login?next=") {
		t.Errorf("HX-Redirect = %q, want /login?next=…", hx)
	}
}

// TestPublicRoutesNeedNoSession: the exemptions are /login, /static and the
// health probes — nothing else.
func TestPublicRoutesNeedNoSession(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.Handler()

	for _, url := range []string{"/login", "/login/totp", "/static/gvm.css", "/static/console.js", "/healthz", "/api/v1/health"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s anonymous = %d, want 200", url, rec.Code)
		}
	}
}

// TestSessionGetsThePage: the happy path still works end to end.
func TestSessionGetsThePage(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest("GET", "/instances", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "pz-main") {
		t.Error("instance list should name the stub instance")
	}
	if rid := rec.Header().Get("X-Request-Id"); rid == "" {
		t.Error("every response must carry X-Request-Id")
	}
}

// TestConsolePageUnknownInstance404s: a mistyped URL must not render a
// live-looking empty console.
func TestConsolePageUnknownInstance404s(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest("GET", "/i/typo/console", nil)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown instance console = %d, want 404", rec.Code)
	}
}

// ── Cross-origin protection (item 3) ─────────────────────────────────────────

// TestCrossOriginFormPostRejected: a forged cross-origin POST is refused even
// with a valid session — this is what replaced per-form CSRF tokens.
func TestCrossOriginFormPostRejected(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.Handler()

	req := withSession(httptest.NewRequest("POST", "/i/pz-main/start", nil))
	req.Host = "dash.example.com"
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST = %d, want 403", rec.Code)
	}
}

// TestSameOriginFormPostAccepted: the same POST from the dashboard's own
// origin flows through to the handler (which answers 303, not 2xx).
func TestSameOriginFormPostAccepted(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.Handler()

	req := withSession(httptest.NewRequest("POST", "/i/pz-main/start", nil))
	req.Host = "dash.example.com"
	req.Header.Set("Origin", "https://dash.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("same-origin POST = %d, want 303", rec.Code)
	}
}

// TestTrustedOriginAccepted: edge.trusted_origins is how a foreign proxy that
// rewrites Host (§17.3) keeps form POSTs working.
func TestTrustedOriginAccepted(t *testing.T) {
	s, _ := newTestServer(t, func(c *Config) {
		c.TrustedOrigins = []string{"https://gvm.example.com"}
	})
	h := s.Handler()

	req := withSession(httptest.NewRequest("POST", "/i/pz-main/start", nil))
	req.Host = "127.0.0.1:8130" // what the app sees when the proxy rewrites Host
	req.Header.Set("Origin", "https://gvm.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("trusted-origin POST = %d, want 303", rec.Code)
	}
}

// TestNewRejectsMalformedTrustedOrigin: a bad origin must fail construction,
// not silently weaken CSRF.
func TestNewRejectsMalformedTrustedOrigin(t *testing.T) {
	_, err := New(Config{Addr: ":0", TrustedOrigins: []string{"not a url"}})
	if err == nil {
		t.Error("New accepted a malformed trusted origin")
	}
}

// ── web-dashboard-13: the degraded path ──────────────────────────────────────

// TestDegradedPageOnNilQuerier: with no store wired, browser pages serve the
// single 503 template — and it names no instance.
func TestDegradedPageOnNilQuerier(t *testing.T) {
	s, _ := newTestServer(t, func(c *Config) { c.InstanceQuerier = nil })
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest("GET", "/instances", nil)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "pz-main") {
		t.Error("the 503 page must carry generic commands only, not a fixture instance name")
	}
}
