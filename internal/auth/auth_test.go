package auth

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/authz"
	"github.com/S95F/gameVPSmanager/internal/identity"
)

// ── Session middleware ────────────────────────────────────────────────────────

type stubSessions struct {
	sessions map[string]*Session
	err      error
	lookups  int
}

func (s *stubSessions) LookupSession(_ context.Context, id string) (*Session, error) {
	s.lookups++
	if s.err != nil {
		return nil, s.err
	}
	if sess, ok := s.sessions[id]; ok {
		return sess, nil
	}
	return nil, ErrSessionNotFound
}

// echoSession replies 200 with the session email, or "anonymous".
func echoSession() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sess, ok := FromContext(r.Context()); ok {
			_, _ = w.Write([]byte(sess.Email))
			return
		}
		_, _ = w.Write([]byte("anonymous"))
	})
}

func TestLoadSessionResolvesCookie(t *testing.T) {
	m := NewMiddleware(&stubSessions{sessions: map[string]*Session{
		"s1": {ID: "s1", Email: "op@example.com", Role: "operator"},
	}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/instances", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "s1"})
	m.LoadSession(echoSession()).ServeHTTP(rec, req)
	if rec.Body.String() != "op@example.com" {
		t.Errorf("body = %q, want the session email", rec.Body.String())
	}
}

func TestLoadSessionStaleCookieClearsAndContinuesAnonymous(t *testing.T) {
	m := NewMiddleware(&stubSessions{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/instances", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "gone"})
	m.LoadSession(echoSession()).ServeHTTP(rec, req)
	if rec.Body.String() != "anonymous" {
		t.Errorf("body = %q, want anonymous continuation", rec.Body.String())
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("stale session cookie was not cleared")
	}
}

func TestLoadSessionStoreDownServesDegraded(t *testing.T) {
	m := NewMiddleware(&stubSessions{err: ErrStoreUnavailable})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/instances", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "s1"})
	m.LoadSession(echoSession()).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("NOT")) {
		t.Error("503 body should state game servers are not affected")
	}
}

func TestRequireAuthRedirectsWithNext(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/i/pz-main/console?tail=50", nil)
	RequireAuth(echoSession()).ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login?next=") || !strings.Contains(loc, "pz-main") {
		t.Errorf("Location = %q, want /login?next= carrying the original URL", loc)
	}
}

func TestRequireAuthHtmxGets401WithHXRedirect(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/instances", nil)
	req.Header.Set("HX-Request", "true")
	RequireAuth(echoSession()).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a 303 would swap the login page into a fragment)", rec.Code)
	}
	if hx := rec.Header().Get("HX-Redirect"); !strings.HasPrefix(hx, "/login?next=") {
		t.Errorf("HX-Redirect = %q, want /login?next=…", hx)
	}
}

// ── Bearer middleware ────────────────────────────────────────────────────────

type stubTokens struct {
	byHash  map[string]*APIToken // key: string(sha256)
	err     error
	usedIDs []string
}

func (s *stubTokens) LookupAPIToken(_ context.Context, sha []byte) (*APIToken, error) {
	if s.err != nil {
		return nil, s.err
	}
	if tok, ok := s.byHash[string(sha)]; ok {
		return tok, nil
	}
	return nil, ErrTokenNotFound
}

func (s *stubTokens) RecordTokenUse(_ context.Context, id string, _ time.Time, _ string) error {
	s.usedIDs = append(s.usedIDs, id)
	return nil
}

// echoActor replies with the effective role of the token actor.
func echoActor() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := TokenActorFromContext(r.Context())
		if !ok {
			http.Error(w, "no actor", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(actor.Type + ":" + string(actor.Role)))
	})
}

func bearerRequest(token string) *http.Request {
	req := httptest.NewRequest("GET", "/api/v1/instances", nil)
	req.Header.Set("Authorization", BearerPrefix+token)
	return req
}

func TestBearerVerifiesByHashAndAppliesCeiling(t *testing.T) {
	const plaintext = "gvmt_test-token"
	stub := &stubTokens{byHash: map[string]*APIToken{
		string(identity.HashToken(plaintext)): {
			ID: "tok1", UserID: "u1", Name: "ci",
			RoleCeiling: authz.RoleViewer, UserRole: authz.RoleAdmin,
		},
	}}
	rec := httptest.NewRecorder()
	NewBearer(stub).Wrap(echoActor()).ServeHTTP(rec, bearerRequest(plaintext))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	// An admin holding a viewer-ceiling token acts as a viewer, audited as
	// api_token.
	if got := rec.Body.String(); got != "api_token:viewer" {
		t.Errorf("actor = %q, want api_token:viewer (ceiling applied)", got)
	}
	if len(stub.usedIDs) != 1 || stub.usedIDs[0] != "tok1" {
		t.Errorf("last_used_at not recorded: %v", stub.usedIDs)
	}
}

func TestBearerRejections(t *testing.T) {
	const plaintext = "gvmt_test-token"
	hash := string(identity.HashToken(plaintext))
	past := time.Now().Add(-time.Hour)

	cases := []struct {
		name   string
		tok    *APIToken
		status int
	}{
		{"revoked", &APIToken{ID: "t", RevokedAt: &past, RoleCeiling: authz.RoleViewer, UserRole: authz.RoleViewer}, http.StatusUnauthorized},
		{"expired", &APIToken{ID: "t", ExpiresAt: &past, RoleCeiling: authz.RoleViewer, UserRole: authz.RoleViewer}, http.StatusUnauthorized},
		{"user disabled", &APIToken{ID: "t", UserDisabled: true, RoleCeiling: authz.RoleViewer, UserRole: authz.RoleViewer}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubTokens{byHash: map[string]*APIToken{hash: tc.tok}}
			rec := httptest.NewRecorder()
			NewBearer(stub).Wrap(echoActor()).ServeHTTP(rec, bearerRequest(plaintext))
			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.status)
			}
			if len(stub.usedIDs) != 0 {
				t.Error("a refused token must not be stamped as used")
			}
		})
	}
}

func TestBearerUnknownTokenIs401(t *testing.T) {
	stub := &stubTokens{byHash: map[string]*APIToken{}}
	rec := httptest.NewRecorder()
	NewBearer(stub).Wrap(echoActor()).ServeHTTP(rec, bearerRequest("gvmt_wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 should carry a WWW-Authenticate challenge")
	}
}

func TestBearerMissingHeaderIs401(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/instances", nil)
	// A session cookie is NOT a bearer token (D-40).
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "s1"})
	NewBearer(&stubTokens{}).Wrap(echoActor()).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestBearerStoreDownIsDegraded(t *testing.T) {
	stub := &stubTokens{err: ErrStoreUnavailable}
	rec := httptest.NewRecorder()
	NewBearer(stub).Wrap(echoActor()).ServeHTTP(rec, bearerRequest("gvmt_x"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
