package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestIDEqualsHeaderAndEnvelope pins §16.3's invariant: the request_id
// in the envelope IS the X-Request-Id header, so one string finds the audit row.
func TestRequestIDEqualsHeaderAndEnvelope(t *testing.T) {
	var envelope Envelope
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.StatusNotFound, CodeNotFound, "no such thing", nil)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/instances/x/status", nil))

	header := rec.Header().Get(RequestIDHeader)
	if header == "" {
		t.Fatal("no X-Request-Id header set")
	}
	if !strings.HasPrefix(header, "req_") || len(header) != len("req_")+16 {
		t.Errorf("request id %q is not req_ + 16 chars", header)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	if envelope.Error.RequestID != header {
		t.Errorf("envelope request_id %q != header %q", envelope.Error.RequestID, header)
	}
	if envelope.Error.Code != CodeNotFound {
		t.Errorf("code = %q, want not_found", envelope.Error.Code)
	}
}

// TestRequestIDsAreUnique would catch the rand fallback becoming the norm.
func TestRequestIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newRequestID()
		if seen[id] {
			t.Fatalf("duplicate request id %q", id)
		}
		seen[id] = true
	}
}

// TestDegradedPageNamesNoInstance is web-dashboard-13: the single 503 template
// carries generic commands only. The fixture names are the example instances
// used across this repo's tests and docs; none may appear.
func TestDegradedPageNamesNoInstance(t *testing.T) {
	fixtureNames := []string{"pz-main", "pz1", "pz2", "valheim-main", "mc-main"}

	rec := httptest.NewRecorder()
	ServeDegraded(rec, httptest.NewRequest("GET", "/instances", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("browser 503 status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, name := range fixtureNames {
		if strings.Contains(body, name) {
			t.Errorf("503 page contains instance name %q; it must carry generic commands only", name)
		}
	}
	for _, generic := range []string{"sudo gvm status", "sudo gvm doctor"} {
		if !strings.Contains(body, generic) {
			t.Errorf("503 page should offer %q", generic)
		}
	}
}

// TestDegradedAPIIsEnvelope: the same entry point speaks JSON on /api/ paths.
func TestDegradedAPIIsEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	ServeDegraded(rec, httptest.NewRequest("GET", "/api/v1/instances", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("API 503 status = %d", rec.Code)
	}
	var envelope Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("API 503 body is not an envelope: %v", err)
	}
	if envelope.Error.Code != CodeDegraded {
		t.Errorf("code = %q, want degraded", envelope.Error.Code)
	}
}

// TestCodesAreClosedAndDistinct keeps the set §16.3 defines: present, unique,
// snake_case.
func TestCodesAreClosedAndDistinct(t *testing.T) {
	want := []Code{
		"unauthenticated", "forbidden", "not_found", "conflict",
		"confirmation_required", "plan_expired", "precondition_failed",
		"rate_limited", "degraded", "internal", "not_implemented",
	}
	got := Codes()
	if len(got) != len(want) {
		t.Fatalf("Codes() has %d entries, want %d", len(got), len(want))
	}
	seen := map[Code]bool{}
	for i, c := range got {
		if seen[c] {
			t.Errorf("duplicate code %q", c)
		}
		seen[c] = true
		if c != want[i] {
			t.Errorf("Codes()[%d] = %q, want %q", i, c, want[i])
		}
	}
}
