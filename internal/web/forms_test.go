package web

// Item 3's third test: every form a browser page renders posts into the
// browser mux — never into /api/v1, whose mux would 401 the cookie-bearing
// browser (D-40).

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var formActionRE = regexp.MustCompile(`action="([^"]+)"`)

// patternToRE turns a mux pattern into an exact-match regexp:
// "/i/{name}/start" → ^/i/[^/]+/start$.
func patternToRE(pattern string) *regexp.Regexp {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			parts[i] = "[^/]+"
		} else {
			parts[i] = regexp.QuoteMeta(p)
		}
	}
	return regexp.MustCompile("^" + strings.Join(parts, "/") + "$")
}

func TestEveryBrowserFormPostsIntoBrowserMux(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.Handler()

	var postPatterns []*regexp.Regexp
	for _, rt := range browserRoutes {
		if rt.method == "POST" {
			postPatterns = append(postPatterns, patternToRE(rt.pattern))
		}
	}

	pages := []struct {
		url         string
		withSession bool
	}{
		{"/login", false},
		{"/login/totp", false},
		{"/instances", true},
		{"/i/pz-main", true},
		{"/i/pz-main/console", true},
	}

	sawForm := false
	for _, page := range pages {
		req := httptest.NewRequest("GET", page.url, nil)
		if page.withSession {
			req = withSession(req)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d, want 200", page.url, rec.Code)
		}

		for _, m := range formActionRE.FindAllStringSubmatch(rec.Body.String(), -1) {
			action := m[1]
			sawForm = true
			if strings.HasPrefix(action, "/api/") {
				t.Errorf("%s renders a form posting to %q: the API mux ignores cookies and would 401 it", page.url, action)
				continue
			}
			matched := false
			for _, re := range postPatterns {
				if re.MatchString(action) {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("%s renders a form posting to %q, which no browser POST route serves", page.url, action)
			}
		}
	}
	if !sawForm {
		t.Fatal("no form found on any page; the test matched nothing")
	}
}

// TestSSERoutesLiveUnderStreamPrefix is D-38: every text/event-stream route
// sits under a reserved /stream/ prefix, so the Caddy stream matcher
// (flush_interval -1) can never drift as routes grow.
func TestSSERoutesLiveUnderStreamPrefix(t *testing.T) {
	var sseCount int
	for _, rt := range browserRoutes {
		if rt.sse {
			sseCount++
			if !strings.HasPrefix(rt.pattern, "/stream/") {
				t.Errorf("browser SSE route %q is outside /stream/ (D-38)", rt.pattern)
			}
		}
	}
	for _, rt := range apiRoutes {
		if rt.sse {
			sseCount++
			if !strings.HasPrefix(rt.pattern, "/api/v1/stream/") {
				t.Errorf("API SSE route %q is outside /api/v1/stream/ (D-38)", rt.pattern)
			}
		}
	}
	if sseCount != 2 {
		t.Errorf("expected exactly 2 SSE routes (browser + API console), found %d", sseCount)
	}
}

// TestNoInlineStyleOrScript keeps the pages inside the shipped CSP
// (default-src 'self', no unsafe-*): no inline <style>, no <script> without
// src, no style= attributes (§16.5's csp constraints).
func TestNoInlineStyleOrScript(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.Handler()

	inlineScript := regexp.MustCompile(`<script(>|[^>]*[^/]>)`)
	for _, url := range []string{"/login", "/instances", "/i/pz-main", "/i/pz-main/console"} {
		req := httptest.NewRequest("GET", url, nil)
		if url != "/login" {
			req = withSession(req)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if strings.Contains(body, "<style") {
			t.Errorf("%s has an inline <style>; the CSP (style-src 'self') blocks it", url)
		}
		if strings.Contains(body, " style=") {
			t.Errorf("%s has a style= attribute; the CSP blocks it", url)
		}
		for _, m := range inlineScript.FindAllString(body, -1) {
			if !strings.Contains(m, "src=") {
				t.Errorf("%s has an inline <script>; the CSP (default-src 'self') blocks it", url)
			}
		}
	}
}
