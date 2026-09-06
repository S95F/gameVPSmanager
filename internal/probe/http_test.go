package probe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPProbeAcceptsA200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	p := &HTTP{URL: srv.URL + "/healthz", Port: "edge"}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.Last == nil || p.Last.Status != 200 {
		t.Fatalf("Last = %+v, want a recorded 200", p.Last)
	}
	if p.Last.Body != `{"status":"ok"}` {
		t.Errorf("body = %q", p.Last.Body)
	}
}

// A status mismatch is a distinct sentinel because the server is UP: the
// operator's next move is to look at what it is saying, not at whether it is
// running.
func TestHTTPProbeRejectsA503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := &HTTP{URL: srv.URL}
	err := p.Run(context.Background())
	if !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("err = %v, want ErrHTTPStatus", err)
	}
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("error does not wrap ErrNotReady: %v", err)
	}
	if p.Last == nil || p.Last.Status != 503 {
		t.Errorf("Last = %+v; a failing probe still records what it saw", p.Last)
	}
}

// A probe that follows redirects reports the status of whatever it was
// redirected to, which on a misconfigured reverse proxy is a login page on
// another host answering 200 while the service behind it is down.
func TestHTTPProbeDoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write([]byte("a completely different service"))
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	p := &HTTP{URL: srv.URL}
	if err := p.Run(context.Background()); !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("err = %v, want the 302 to be reported rather than followed", err)
	}
	if p.Last.Status != http.StatusFound {
		t.Errorf("status = %d, want 302", p.Last.Status)
	}
	if got := elsewhere.Load(); got != 0 {
		t.Errorf("the redirect target was fetched %d times; it must not be", got)
	}
	// A caller that genuinely wants the redirect says so.
	p.ExpectStatus = []int{http.StatusFound}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("with ExpectStatus 302: %v", err)
	}
}

// "nginx is up" and "the app behind nginx is up" are different facts, and only
// a body check tells them apart.
func TestHTTPProbeExpectBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>default nginx page</html>"))
	}))
	defer srv.Close()

	p := &HTTP{URL: srv.URL, ExpectBody: "gvm-edge"}
	err := p.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with the marker absent from the body")
	}
	if !strings.Contains(err.Error(), "gvm-edge") {
		t.Errorf("err = %v, want it to name the missing marker", err)
	}

	p.ExpectBody = "nginx"
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("with a marker that is present: %v", err)
	}
}

// A readiness check needs a marker, not a page: the cap is what stops a panel
// that answers with a 40 MB dashboard from turning every poll into a 40 MB
// allocation.
func TestHTTPProbeCapsTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		big := make([]byte, maxHTTPBody*2)
		for i := range big {
			big[i] = 'x'
		}
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	p := &HTTP{URL: srv.URL}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.Last.Body) != maxHTTPBody {
		t.Fatalf("kept %d body bytes, want the %d byte cap", len(p.Last.Body), maxHTTPBody)
	}
}

// net/http reads Host off the Request struct and ignores a map entry, so a
// caller setting it in Header would silently get no virtual-host routing.
func TestHTTPProbeSendsAHostHeader(t *testing.T) {
	var seen atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Host)
	}))
	defer srv.Close()

	p := &HTTP{
		URL:    srv.URL,
		Header: http.Header{"Host": []string{"panel.example.test"}, "X-Gvm": []string{"1"}},
	}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, _ := seen.Load().(string); got != "panel.example.test" {
		t.Fatalf("server saw Host %q, want the override", got)
	}
}

func TestHTTPProbeFailsAgainstADeadEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	p := &HTTP{URL: url, Timeout: time.Second}
	err := p.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded against a closed listener")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("error does not wrap ErrNotReady: %v", err)
	}
}

// The probe's own timeout has to bound a server that accepts the connection
// and then says nothing, which is what a game panel does while it is starting.
func TestHTTPProbeAppliesItsOwnTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	p := &HTTP{URL: srv.URL, Timeout: 100 * time.Millisecond}
	started := time.Now()
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded against a server that never answered")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("took %s; the probe's own timeout did not apply", elapsed)
	}
}

// The accept rule is a free function so the whole table is provable without a
// server: 3xx must not pass by default (it would pass on a redirect to an
// error page) and neither must 4xx (it would pass on "not found").
func TestCheckHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		code int
		want []int
		ok   bool
	}{
		{200, nil, true},
		{204, nil, true},
		{299, nil, true},
		{300, nil, false},
		{302, nil, false},
		{404, nil, false},
		{500, nil, false},
		{199, nil, false},
		{401, []int{401}, true},
		{401, []int{200, 403}, false},
	} {
		err := checkHTTPStatus(tc.code, tc.want)
		if (err == nil) != tc.ok {
			t.Errorf("checkHTTPStatus(%d, %v) = %v, want ok=%v", tc.code, tc.want, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrHTTPStatus) {
			t.Errorf("checkHTTPStatus(%d, %v) does not wrap ErrHTTPStatus", tc.code, tc.want)
		}
	}
}

func TestHTTPDescribe(t *testing.T) {
	p := &HTTP{URL: "http://127.0.0.1:8080/healthz", Port: "edge",
		ExpectStatus: []int{200, 204}, ExpectBody: "ok", Method: http.MethodHead}
	got := p.Describe()
	for _, want := range []string{"HEAD", "http://127.0.0.1:8080/healthz", "200 or 204", `"ok"`, "edge"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, want it to mention %q", got, want)
		}
	}
	plain := (&HTTP{URL: "http://x/"}).Describe()
	if !strings.Contains(plain, "GET") || !strings.Contains(plain, "2xx") {
		t.Errorf("Describe() = %q, want the GET/2xx defaults spelled out", plain)
	}
}

func TestHTTPProbeRejectsABadURL(t *testing.T) {
	p := &HTTP{URL: "://not a url"}
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded with an unparseable URL")
	}
}
