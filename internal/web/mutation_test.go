package web

// U3 over HTTP: in this build no mutation route may return 2xx, because
// nothing dispatches to gvmd yet (web-dashboard-2, day2-ops-6).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNoMutationReturns2xxWithoutDispatch wires a recording fake gvmd client,
// posts every mutation route on both muxes, and asserts that none answered
// 2xx and none dispatched. The browser routes answer 303 with a flash; the
// API routes answer 501 with the not_implemented envelope.
func TestNoMutationReturns2xxWithoutDispatch(t *testing.T) {
	fake := &recordingDispatcher{}
	s, _ := newTestServer(t, func(c *Config) { c.Dispatcher = fake })
	h := s.Handler()

	for _, rt := range browserRoutes {
		if rt.method != "POST" || !strings.HasPrefix(rt.pattern, "/i/") {
			continue
		}
		t.Run("browser "+rt.pattern, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, withSession(httptest.NewRequest("POST", patternURL(rt.pattern), nil)))
			if rec.Code >= 200 && rec.Code < 300 {
				t.Fatalf("status = %d: a 2xx without a dispatch is fake success", rec.Code)
			}
			if rec.Code != http.StatusSeeOther {
				t.Errorf("status = %d, want 303 with a flash (§16.3 form POST)", rec.Code)
			}
			// The flash must carry the honest refusal and the CLI fallback.
			flash := ""
			for _, c := range rec.Result().Cookies() {
				if c.Name == flashCookie && c.MaxAge >= 0 {
					flash = c.Value
				}
			}
			if flash == "" {
				t.Error("no flash message was set")
			}
		})
	}

	for _, rt := range apiRoutes {
		if rt.method != "POST" {
			continue
		}
		t.Run("api "+rt.pattern, func(t *testing.T) {
			req := httptest.NewRequest("POST", patternURL(rt.pattern), nil)
			req.Header.Set("Authorization", "Bearer "+testToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code >= 200 && rec.Code < 300 {
				t.Fatalf("status = %d: a 2xx without a dispatch is fake success", rec.Code)
			}
			if rec.Code != http.StatusNotImplemented {
				t.Errorf("status = %d, want 501", rec.Code)
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

	if len(fake.calls) != 0 {
		t.Errorf("the recording gvmd client saw %v; nothing may dispatch in this build", fake.calls)
	}
}

// TestDetailPageHidesLifecycleButtons: a button that cannot act must not
// render (item 1: "the buttons are hidden").
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
			t.Errorf("detail page renders a form for %s; dispatch is not in this build", action)
		}
	}
}
