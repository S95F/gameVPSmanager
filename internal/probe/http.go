package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// KindHTTP is this probe's kind string.
//
// It is NOT in internal/spec's closed probe-kind registry, and that is not an
// oversight: no shipped manifest declares an HTTP readiness check, because no
// game in the v1 set serves one. The probe exists for the surfaces that do --
// the edge's own health endpoint (section 17) and doctor's reachability check
// -- and admitting it into the manifest grammar is a deliberate one-line
// registry change plus a manifest revision bump, not something a probe file
// gets to do to the grammar on its own.
const KindHTTP = "http_probe"

// maxHTTPBody is how much of a response body the probe will read.
//
// A readiness check needs a marker, not a page. The cap is what stops a game
// panel that answers the health path with a 40 MB HTML dashboard from turning
// every poll into a 40 MB allocation.
const maxHTTPBody = 64 << 10

// DefaultHTTPTimeout bounds one request when neither the probe nor the caller
// names a deadline. It is short because an HTTP endpoint that has not answered
// in five seconds has told the probe what it needs to know.
const DefaultHTTPTimeout = 5 * time.Second

// ErrHTTPStatus is returned when the response arrived but its status is not one
// the probe accepts. It is a distinct sentinel because a status mismatch means
// the server is UP and answering -- the operator's next move is to look at what
// it is saying, not at whether it is running.
var ErrHTTPStatus = errors.New("probe: unexpected HTTP status")

// HTTPResult is the last answer, for the status line.
type HTTPResult struct {
	Status  int
	Elapsed time.Duration
	Body    string // truncated to maxHTTPBody
}

// HTTP reports a target ready when it answers an HTTP request with an accepted
// status.
//
// Two decisions are worth stating because both are easy to get wrong in a way
// that only shows up in production:
//
//   - Redirects are NOT followed. A probe that follows them reports the status
//     of whatever it was redirected to, which on a misconfigured reverse proxy
//     is a login page on a different host answering 200 while the service
//     behind it is down. The status this probe reports is the status the
//     address it was pointed at actually returned.
//
//   - The body is read and discarded even when no ExpectBody is set. Leaving
//     an unread body behind holds the connection out of the pool and, on a
//     server with a small worker count, a readiness loop that leaks one
//     connection every two seconds is a readiness loop that eventually causes
//     the outage it was watching for.
type HTTP struct {
	// Port is the manifest port NAME, for the description.
	Port string

	// URL is the absolute URL to request.
	URL string

	// Method defaults to GET. HEAD is the polite choice against an endpoint
	// that serves a real page, but not every server implements it.
	Method string

	// Header is sent with the request. A Host entry is honoured as the Host
	// header, which is how a probe reaches a name-based virtual host over a
	// loopback address.
	Header http.Header

	// ExpectStatus is the set of accepted status codes. Empty means any 2xx:
	// a readiness probe that accepted 3xx would pass on a redirect to an error
	// page, and one that accepted 4xx would pass on "not found".
	ExpectStatus []int

	// ExpectBody, when set, must appear in the first maxHTTPBody bytes of the
	// response. This is what distinguishes "nginx is up" from "the app behind
	// nginx is up".
	ExpectBody string

	// Timeout bounds one request. Zero means DefaultHTTPTimeout, or the
	// caller's deadline when it is tighter.
	Timeout time.Duration

	// Client is the seam for tests and for a probe that must go through a
	// unix socket transport. nil builds a client that does not follow
	// redirects.
	Client *http.Client

	// Last is the most recent answer, successful or not.
	Last *HTTPResult
}

var _ Probe = (*HTTP)(nil)

// Kind implements Probe.
func (p *HTTP) Kind() string { return KindHTTP }

// Describe implements Probe.
func (p *HTTP) Describe() string {
	what := "an HTTP " + p.method() + " to " + p.URL
	if len(p.ExpectStatus) > 0 {
		codes := make([]string, 0, len(p.ExpectStatus))
		for _, c := range p.ExpectStatus {
			codes = append(codes, strconv.Itoa(c))
		}
		what += " answering " + strings.Join(codes, " or ")
	} else {
		what += " answering 2xx"
	}
	if p.ExpectBody != "" {
		what += " with " + strconv.Quote(p.ExpectBody) + " in the body"
	}
	if p.Port != "" {
		what += " (port " + p.Port + ")"
	}
	return what
}

func (p *HTTP) method() string {
	if p.Method == "" {
		return http.MethodGet
	}
	return p.Method
}

// noRedirectClient is the default: see the type comment for why following a
// redirect makes a readiness probe lie.
var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Run implements Probe.
func (p *HTTP) Run(ctx context.Context) error {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultHTTPTimeout
	}
	ctx, cancel := withTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, p.method(), p.URL, nil)
	if err != nil {
		return fail(KindHTTP, p.URL, "cannot build the request", err)
	}
	for k, vs := range p.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
		// Host is not a normal header: net/http reads it off the Request
		// struct and ignores the map entry, so a caller setting it in Header
		// would silently get no virtual-host routing at all.
		if http.CanonicalHeaderKey(k) == "Host" && len(vs) > 0 {
			req.Host = vs[0]
		}
	}

	client := p.Client
	if client == nil {
		client = noRedirectClient
	}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fail(KindHTTP, p.URL, "request failed", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
	// Drained and closed on every path, including the failing ones.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxHTTPBody))
	_ = resp.Body.Close()

	res := &HTTPResult{Status: resp.StatusCode, Elapsed: time.Since(started), Body: string(body)}
	p.Last = res

	if err := checkHTTPStatus(resp.StatusCode, p.ExpectStatus); err != nil {
		return fail(KindHTTP, p.URL, "", err)
	}
	if readErr != nil {
		return fail(KindHTTP, p.URL, "reading the body", readErr)
	}
	if p.ExpectBody != "" && !strings.Contains(res.Body, p.ExpectBody) {
		return fail(KindHTTP, p.URL,
			"status "+strconv.Itoa(resp.StatusCode)+" but the body does not contain "+
				strconv.Quote(p.ExpectBody), nil)
	}
	return nil
}

// checkHTTPStatus is the accept rule, as a free function so the table of what
// passes and what does not is tested without a server.
func checkHTTPStatus(code int, want []int) error {
	if len(want) == 0 {
		if code >= 200 && code < 300 {
			return nil
		}
		return fmt.Errorf("%w: %d, want 2xx", ErrHTTPStatus, code)
	}
	for _, w := range want {
		if code == w {
			return nil
		}
	}
	return fmt.Errorf("%w: %d, want one of %v", ErrHTTPStatus, code, want)
}
