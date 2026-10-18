package web

// U3 over HTTP, both directions. Without a Dispatcher no mutation route may
// return 2xx (web-dashboard-2, day2-ops-6); with one, a 2xx means gvmd really
// acknowledged a dispatch, a held lease is 409, and a role below operator is
// refused before anything is dispatched (§15.2).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// browserMutations and apiMutations walk the route tables so a new mutation
// route cannot dodge these tests. The console send box is a mutation too, but
// it has its own validation surface, tested in console_test.go.
func browserMutations() []browserRoute {
	var out []browserRoute
	for _, rt := range browserRoutes {
		if rt.method != "POST" || !strings.HasPrefix(rt.pattern, "/i/") {
			continue
		}
		if strings.HasSuffix(rt.pattern, "/console") {
			continue
		}
		out = append(out, rt)
	}
	return out
}

func apiMutations() []apiRoute {
	var out []apiRoute
	for _, rt := range apiRoutes {
		if rt.method == "POST" {
			out = append(out, rt)
		}
	}
	return out
}

func TestNoMutationReturns2xxWithoutDispatch(t *testing.T) {
	s, _ := newTestServer(t, nil) // no Dispatcher wired
	h := s.Handler()

	for _, rt := range browserMutations() {
		t.Run("browser "+rt.pattern, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, withSession(httptest.NewRequest("POST", patternURL(rt.pattern), nil)))
			if rec.Code >= 200 && rec.Code < 300 {
				t.Fatalf("status = %d: a 2xx without a dispatch is fake success", rec.Code)
			}
			if rec.Code != http.StatusSeeOther {
				t.Errorf("status = %d, want 303 with a flash (§16.3 form POST)", rec.Code)
			}
			flash := flashValue(rec)
			if flash == "" {
				t.Error("no flash message was set")
			}
		})
	}

	for _, rt := range apiMutations() {
		t.Run("api "+rt.pattern, func(t *testing.T) {
			req := httptest.NewRequest("POST", patternURL(rt.pattern), nil)
			req.Header.Set("Authorization", "Bearer "+testToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501", rec.Code)
			}
			var envelope struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("501 body is not an envelope: %v", err)
			}
			if envelope.Error.Code != "not_implemented" {
				t.Errorf("code = %q, want not_implemented", envelope.Error.Code)
			}
			if !strings.Contains(envelope.Error.Message, "sudo gvm") {
				t.Errorf("message %q should hand the operator the CLI command", envelope.Error.Message)
			}
		})
	}
}

// Test2xxMeansDispatched: with a Dispatcher wired and an operator actor,
// every mutation route acknowledges only what gvmd acknowledged — the 202 (or
// the browser flash) carries the op id the fake returned.
func Test2xxMeansDispatched(t *testing.T) {
	fake := &recordingDispatcher{}
	s, _ := newTestServer(t, func(c *Config) { c.Dispatcher = fake })
	h := s.Handler()

	for _, rt := range browserMutations() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withSession(httptest.NewRequest("POST", patternURL(rt.pattern), nil)))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: status = %d, want 303", rt.pattern, rec.Code)
		}
		if flash := flashValue(rec); !strings.Contains(flash, "op_test") {
			t.Errorf("%s: flash %q should carry the op id", rt.pattern, flash)
		}
	}

	for _, rt := range apiMutations() {
		req := httptest.NewRequest("POST", patternURL(rt.pattern), nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: status = %d, want 202; body: %s", rt.pattern, rec.Code, rec.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["op_id"] != "op_test" || body["status"] != "accepted" {
			t.Errorf("%s: body = %v", rt.pattern, body)
		}
	}

	// 3 browser + 3 API dispatches, each recorded with its actor.
	if len(fake.calls) != 6 {
		t.Errorf("dispatches = %v, want 6", fake.calls)
	}
	for _, call := range fake.calls {
		if !strings.Contains(call, "user:op@example.com") && !strings.Contains(call, "api_token:ci") {
			t.Errorf("dispatch %q carries no actor for the audit row", call)
		}
	}
}

// TestLeaseRefusalIs409: a held instance answers 409 with the lease line on
// the API and a flash carrying the line in the browser — and no op id.
func TestLeaseRefusalIs409(t *testing.T) {
	fake := &recordingDispatcher{err: &LeaseHeldError{Line: "backup running since 05:00"}}
	s, _ := newTestServer(t, func(c *Config) { c.Dispatcher = fake })
	h := s.Handler()

	req := httptest.NewRequest("POST", "/api/v1/instances/pz-main/restart", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "backup running since 05:00") {
		t.Error("409 should carry the lease line")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest("POST", "/i/pz-main/restart", nil)))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("browser status = %d, want 303", rec.Code)
	}
	if flash := flashValue(rec); !strings.Contains(flash, "backup running since 05:00") {
		t.Errorf("flash %q should carry the lease line", flash)
	}
	if len(fake.calls) != 0 {
		t.Errorf("a refused dispatch was recorded: %v", fake.calls)
	}
}

// TestViewerMayNotMutate: the §15.2 floor — a viewer session and a
// viewer-ceiling token are refused before anything is dispatched.
func TestViewerMayNotMutate(t *testing.T) {
	fake := &recordingDispatcher{}
	s, _ := newTestServer(t, func(c *Config) { c.Dispatcher = fake })
	h := s.Handler()

	req := httptest.NewRequest("POST", "/i/pz-main/stop", nil)
	req.AddCookie(&http.Cookie{Name: "gvm_sess", Value: "s2"}) // viewer session
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer session stop = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest("POST", "/api/v1/instances/pz-main/stop", nil)
	req.Header.Set("Authorization", "Bearer "+testViewerToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer-ceiling token stop = %d, want 403", rec.Code)
	}

	if len(fake.calls) != 0 {
		t.Errorf("a forbidden mutation was dispatched: %v", fake.calls)
	}
}

// TestDetailPageHidesLifecycleButtons: a button that cannot act must not
// render while no Dispatcher is wired (item 1: "the buttons are hidden").
func TestDetailPageHidesLifecycleButtons(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest("GET", "/i/pz-main", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, action := range []string{"/start", "/stop", "/restart"} {
		if strings.Contains(body, `action="/i/pz-main`+action) {
			t.Errorf("detail page renders a form for %s with no dispatcher wired", action)
		}
	}
}

// flashValue returns the flash cookie set on a response, decoded-ish (the
// value is base64url, but the substring checks only need the raw text).
func flashValue(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == flashCookie && c.MaxAge >= 0 && c.Value != "" {
			b, err := decodeFlash(c.Value)
			if err != nil {
				return c.Value
			}
			return b
		}
	}
	return ""
}
