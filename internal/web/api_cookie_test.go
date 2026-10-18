package web

// D-40's behavioural half: /api/v1/* returns 401 with a valid session cookie
// and no bearer token, on every route. The structural half is that apimux.go
// contains no session reference at all.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPIIgnoresCookies sends a VALID session cookie with no bearer token and
// expects 401 on every authenticated API route — and that the session store
// was never even consulted for them.
func TestAPIIgnoresCookies(t *testing.T) {
	s, sessions := newTestServer(t, nil)
	h := s.Handler()

	for _, rt := range apiRoutes {
		if rt.public {
			continue
		}
		t.Run(rt.method+" "+rt.pattern, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, withSession(httptest.NewRequest(rt.method, patternURL(rt.pattern), nil)))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("session cookie, no bearer: status = %d, want 401", rec.Code)
			}
		})
	}
	if sessions.lookups != 0 {
		t.Errorf("the API mux consulted the session store %d times; it must never read a cookie", sessions.lookups)
	}
}

// TestBearerIsVerified: the happy path through the API mux — a real token,
// verified by hash, reaches the handler.
func TestBearerIsVerified(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.Handler()

	req := httptest.NewRequest("GET", "/api/v1/instances", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pz-main") {
		t.Error("instance list should name the stub instance")
	}

	// And a wrong token is refused.
	req = httptest.NewRequest("GET", "/api/v1/instances", nil)
	req.Header.Set("Authorization", "Bearer gvmt_wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", rec.Code)
	}
}
