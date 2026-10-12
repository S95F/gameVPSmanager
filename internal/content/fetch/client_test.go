package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testNet is a fake internet: several httptest servers reachable under
// DISTINCT HOSTNAMES through the policy's dial seam.
//
// The distinct hostnames are the point. Every httptest server listens on
// 127.0.0.1, so a test that used their URLs directly could not express "this
// host is on the allowlist and that one is not" -- which is exactly the
// property the redirect refusal has to be tested against. The allowlist itself
// is NOT bypassed: the guard in dialAllowed runs first and only then calls this
// dialer, so a host that is refused never gets here.
//
// Nothing in this file reaches the real network. The names are all in the
// RFC 2606 .test namespace, which must never resolve, and the dialer answers
// only names it was given.
type testNet struct {
	t    *testing.T
	srvs map[string]*httptest.Server
}

func newTestNet(t *testing.T) *testNet {
	return &testNet{t: t, srvs: map[string]*httptest.Server{}}
}

// serve puts a handler at a hostname and returns a hit counter for it.
func (n *testNet) serve(host string, h http.HandlerFunc) *atomic.Int64 {
	n.t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	n.t.Cleanup(srv.Close)
	n.srvs[normalizeHost(host)] = srv
	return &hits
}

// policy is a Loopback policy over the named hosts, wired to this fake net.
func (n *testNet) policy(hosts ...string) Policy {
	p := Loopback(hosts...)
	p.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		srv, ok := n.srvs[normalizeHost(addr)]
		if !ok {
			return nil, fmt.Errorf("test net: nothing is listening for %q", addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, srv.Listener.Addr().String())
	}
	return p
}

func (n *testNet) client(p Policy) *Client {
	n.t.Helper()
	c, err := New(p)
	if err != nil {
		n.t.Fatal(err)
	}
	onFakeClock(c)
	return c
}

// onFakeClock puts the client's limiter on a clock that only moves when the
// limiter sleeps.
//
// A server-requested pause is REAL -- a 429 with no Retry-After pauses the
// whole limiter for five seconds, which is the behaviour under test -- so a
// test that sat through three of them would take fifteen seconds and would be
// the first thing somebody deletes. The pause is still taken, still measurable
// through Paused(), and costs nothing. The genuine sleeping is tested directly
// in ratelimit_test.go.
func onFakeClock(c *Client) {
	var mu sync.Mutex
	now := time.Now()
	c.lim.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	c.lim.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
		return nil
	}
}

func TestGetFetchesABoundedBody(t *testing.T) {
	n := newTestNet(t)
	n.serve("api.test", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); !strings.HasPrefix(got, "gvm/") {
			t.Errorf("User-Agent = %q; an anonymous agent is what gets a shared IP "+
				"throttled for everyone on it", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"3171167894"}]}}`)
	})
	c := n.client(n.policy("api.test"))

	var out struct {
		Response struct {
			Details []struct {
				ID string `json:"publishedfileid"`
			} `json:"publishedfiledetails"`
		} `json:"response"`
	}
	if err := c.JSON(context.Background(), "http://api.test/ISteamRemoteStorage/", &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Response.Details) != 1 || out.Response.Details[0].ID != "3171167894" {
		t.Fatalf("decoded %+v", out)
	}
}

func TestPostFormCarriesTheFormSteamsCollectionEndpointNeeds(t *testing.T) {
	n := newTestNet(t)
	n.serve("api.test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("collectioncount") != "1" {
			t.Errorf("form = %v", r.PostForm)
		}
		fmt.Fprint(w, `{"response":{"result":1}}`)
	})
	c := n.client(n.policy("api.test"))

	var out struct {
		Response struct{ Result int } `json:"response"`
	}
	form := url.Values{"collectioncount": {"1"}, "publishedfileids[0]": {"2969704280"}}
	if err := c.PostFormJSON(context.Background(), "http://api.test/GetCollectionDetails/v1/", form, &out); err != nil {
		t.Fatal(err)
	}
	if out.Response.Result != 1 {
		t.Fatalf("result = %d", out.Response.Result)
	}
}

// TestARedirectToAnOffAllowlistHostIsAnErrorNotAFollow is why this package
// exists.
//
// A hostname allowlist whose client follows a 302 anywhere is not an allowlist:
// anyone who can influence a response -- a compromised CDN, a cache poisoner, a
// hijacked project page -- can use one redirect to make gvm fetch mod bytes
// from a host the operator never approved, and those bytes get executed by a
// service account.
//
// The off-allowlist server in this test is REACHABLE: the fake net would answer
// it happily. So the hit counter being zero is evidence about the client's
// decision rather than about the fixture.
func TestARedirectToAnOffAllowlistHostIsAnErrorNotAFollow(t *testing.T) {
	n := newTestNet(t)
	evil := n.serve("off.test", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "mod bytes nobody approved")
	})
	n.serve("cdn.test", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://off.test/evil.jar", http.StatusFound)
	})
	// Only cdn.test is allowed. off.test is listening and is not.
	c := n.client(n.policy("cdn.test"))

	resp, err := c.Get(context.Background(), "http://cdn.test/jei.jar")
	if err == nil {
		t.Fatalf("the redirect was followed and returned %d bytes", len(resp.Body))
	}
	if !errors.Is(err, ErrRedirect) {
		t.Errorf("error does not carry ErrRedirect: %v", err)
	}
	// Both sentinels, because a caller looking for "the allowlist said no"
	// should match on that whether the refusal happened on the first URL or on
	// the fifth hop.
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("error does not carry ErrHostNotAllowed: %v", err)
	}
	if got := evil.Load(); got != 0 {
		t.Fatalf("the off-allowlist host was contacted %d time(s)", got)
	}
	// Terminal: retrying an allowlist is a loop with a delay in it.
	if Classify(err) != ClassTerminal {
		t.Errorf("Classify = %v, want terminal", Classify(err))
	}
	// And the message names the host, because the operator's next question is
	// which one.
	if !strings.Contains(err.Error(), "off.test") {
		t.Errorf("the refusal does not name the host it refused: %v", err)
	}
}

func TestARedirectToAnAllowlistedHostIsFollowed(t *testing.T) {
	n := newTestNet(t)
	n.serve("cdn.test", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://files.test/jei.jar", http.StatusFound)
	})
	n.serve("files.test", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "the approved bytes")
	})
	c := n.client(n.policy("cdn.test", "files.test"))

	resp, err := c.Get(context.Background(), "http://cdn.test/jei.jar")
	if err != nil {
		t.Fatalf("a redirect between two allowlisted hosts must be followed: %v", err)
	}
	if string(resp.Body) != "the approved bytes" {
		t.Errorf("body = %q", resp.Body)
	}
}

func TestARedirectChainIsBounded(t *testing.T) {
	n := newTestNet(t)
	hits := n.serve("loop.test", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://loop.test/again", http.StatusFound)
	})
	p := n.policy("loop.test")
	p.MaxRedirects = 2
	c := n.client(p)

	_, err := c.Get(context.Background(), "http://loop.test/start")
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("an endless chain = %v, want ErrRedirect", err)
	}
	// The first request plus MaxRedirects follows, and then it stops. An
	// unbounded chain is a hang, and a hang inside a download cycle is a
	// server that stays stopped.
	if got := hits.Load(); got > 4 {
		t.Errorf("the chain was followed %d times with MaxRedirects=2", got)
	}
}

func TestARedirectToAnotherSchemeIsRefused(t *testing.T) {
	n := newTestNet(t)
	n.serve("cdn.test", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "file:///etc/shadow")
		w.WriteHeader(http.StatusFound)
	})
	c := n.client(n.policy("cdn.test"))

	_, err := c.Get(context.Background(), "http://cdn.test/jei.jar")
	if !errors.Is(err, ErrRedirect) || !errors.Is(err, ErrScheme) {
		t.Fatalf("a redirect to file:// = %v, want ErrRedirect and ErrScheme", err)
	}
}

// TestTheAllowlistIsEnforcedAtTheSocketToo covers the third layer. The first
// two are policy checks on a URL; this one still holds if a future caller
// reaches past them for the transport.
func TestTheAllowlistIsEnforcedAtTheSocketToo(t *testing.T) {
	n := newTestNet(t)
	n.serve("api.test", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "{}") })
	c := n.client(n.policy("api.test"))

	if _, err := c.dialAllowed(context.Background(), "tcp", "off.test:80"); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("dial off-allowlist = %v, want ErrHostNotAllowed", err)
	}
	if _, err := c.dialAllowed(context.Background(), "tcp", "not-host-port"); !errors.Is(err, ErrPolicy) {
		t.Fatalf("dial a malformed address = %v, want ErrPolicy", err)
	}
	// The control: the allowed host still dials.
	conn, err := c.dialAllowed(context.Background(), "tcp", "api.test:80")
	if err != nil {
		t.Fatalf("dial the allowed host = %v", err)
	}
	conn.Close()
}

// TestNoProxyIsConfigured: http.ProxyFromEnvironment would dial the proxy
// instead of the target, which both defeats the socket-level allowlist (it
// would be judging the proxy's hostname) and is then refused by it. Failing
// closed is correct and baffling, so the decision is explicit.
func TestNoProxyIsConfigured(t *testing.T) {
	c, err := New(SteamPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if c.transport().Proxy != nil {
		t.Fatal("a proxy is configured; the socket-level allowlist would be judging the " +
			"proxy's hostname rather than the target's")
	}
}

func TestAResponseBodyIsBounded(t *testing.T) {
	const bound = 1024

	t.Run("a body longer than the bound", func(t *testing.T) {
		n := newTestNet(t)
		n.serve("api.test", func(w http.ResponseWriter, _ *http.Request) {
			w.Write(bytes.Repeat([]byte("A"), 2<<20))
		})
		p := n.policy("api.test")
		p.MaxResponseBytes = bound
		c := n.client(p)

		_, err := c.Get(context.Background(), "http://api.test/big")
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("a 2 MiB body against a %d-byte bound = %v, want ErrTooLarge", bound, err)
		}
		if Classify(err) != ClassTerminal {
			t.Error("ErrTooLarge must be terminal: asking again gets the same response")
		}
	})

	t.Run("a lying Content-Length costs one round trip", func(t *testing.T) {
		n := newTestNet(t)
		n.serve("api.test", func(w http.ResponseWriter, _ *http.Request) {
			// Declared enormous. Checked before a byte of body is read, so a
			// hostile 10 GB declaration does not cost 10 GB of bandwidth.
			w.Header().Set("Content-Length", "10737418240")
			w.WriteHeader(http.StatusOK)
		})
		p := n.policy("api.test")
		p.MaxResponseBytes = bound
		c := n.client(p)

		_, err := c.Get(context.Background(), "http://api.test/liar")
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("a 10 GB Content-Length = %v, want ErrTooLarge", err)
		}
		if !strings.Contains(err.Error(), "declared") {
			t.Errorf("the refusal does not say it was the declaration: %v", err)
		}
	})

	t.Run("a chunked body with no length is still bounded", func(t *testing.T) {
		n := newTestNet(t)
		n.serve("api.test", func(w http.ResponseWriter, _ *http.Request) {
			f, _ := w.(http.Flusher)
			for range 64 {
				w.Write(bytes.Repeat([]byte("B"), 4096))
				if f != nil {
					f.Flush()
				}
			}
		})
		p := n.policy("api.test")
		p.MaxResponseBytes = bound
		c := n.client(p)

		if _, err := c.Get(context.Background(), "http://api.test/chunked"); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("a chunked body = %v, want ErrTooLarge", err)
		}
	})

	t.Run("a gzip bomb is bounded after decompression", func(t *testing.T) {
		// The bound has to be on the DECODED stream. 4 MiB of zeros gzips to a
		// few kilobytes, so a bound applied to the compressed length would let
		// it straight through.
		var compressed bytes.Buffer
		zw := gzip.NewWriter(&compressed)
		if _, err := zw.Write(bytes.Repeat([]byte{0}, 4<<20)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if compressed.Len() > 64<<10 {
			t.Fatalf("the fixture compressed to %d bytes, which is not a bomb", compressed.Len())
		}

		n := newTestNet(t)
		n.serve("api.test", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Type", "application/json")
			w.Write(compressed.Bytes())
		})
		p := n.policy("api.test")
		p.MaxResponseBytes = bound
		c := n.client(p)

		if _, err := c.Get(context.Background(), "http://api.test/bomb"); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("a gzip bomb = %v, want ErrTooLarge; the bound must be on the decoded "+
				"stream", err)
		}
	})

	t.Run("a body exactly at the bound is accepted", func(t *testing.T) {
		// Off-by-one in the safe direction is still a bug: it would refuse a
		// legitimate response whose size happened to equal the limit.
		n := newTestNet(t)
		n.serve("api.test", func(w http.ResponseWriter, _ *http.Request) {
			w.Write(bytes.Repeat([]byte("C"), bound))
		})
		p := n.policy("api.test")
		p.MaxResponseBytes = bound
		c := n.client(p)

		resp, err := c.Get(context.Background(), "http://api.test/exact")
		if err != nil {
			t.Fatalf("a body of exactly %d bytes = %v", bound, err)
		}
		if len(resp.Body) != bound {
			t.Errorf("read %d bytes", len(resp.Body))
		}
	})
}

func TestStreamIsBoundedAndTheSinkIsNeverFedTwice(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte("PK\x03\x04"), 4096)

	t.Run("the happy path hands every byte to the sink", func(t *testing.T) {
		n := newTestNet(t)
		n.serve("cdn.test", func(w http.ResponseWriter, _ *http.Request) { w.Write(payload) })
		c := n.client(n.policy("cdn.test"))

		var got bytes.Buffer
		err := c.Stream(ctx, "http://cdn.test/jei.jar", int64(len(payload)), func(r io.Reader) error {
			_, err := io.Copy(&got, r)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), payload) {
			t.Errorf("streamed %d bytes, want %d", got.Len(), len(payload))
		}
	})

	t.Run("a body longer than the plan declared", func(t *testing.T) {
		n := newTestNet(t)
		hits := n.serve("cdn.test", func(w http.ResponseWriter, _ *http.Request) {
			// No Content-Length, so the declaration check cannot catch it and
			// the reader has to.
			f, _ := w.(http.Flusher)
			for range 8 {
				w.Write(payload)
				if f != nil {
					f.Flush()
				}
			}
		})
		c := n.client(n.policy("cdn.test"))

		err := c.Stream(ctx, "http://cdn.test/jei.jar", 100, func(r io.Reader) error {
			_, err := io.Copy(io.Discard, r)
			return err
		})
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("Stream of an oversized body = %v, want ErrTooLarge", err)
		}
		if got := hits.Load(); got != 1 {
			t.Errorf("the download was attempted %d times; once the sink has consumed bytes "+
				"a retry would feed it a second copy from byte zero", got)
		}
	})

	t.Run("a sink that fails is not retried", func(t *testing.T) {
		n := newTestNet(t)
		hits := n.serve("cdn.test", func(w http.ResponseWriter, _ *http.Request) { w.Write(payload) })
		c := n.client(n.policy("cdn.test"))

		boom := errors.New("no space left on device")
		err := c.Stream(ctx, "http://cdn.test/jei.jar", 1<<20, func(r io.Reader) error {
			io.CopyN(io.Discard, r, 16)
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("Stream = %v, want the sink's error", err)
		}
		if got := hits.Load(); got != 1 {
			t.Errorf("a failed sink was retried %d times; the second attempt would hash a "+
				"concatenation of two downloads and report a tampered file", got)
		}
	})

	t.Run("a transient status before the body IS retried", func(t *testing.T) {
		n := newTestNet(t)
		var seen atomic.Int64
		n.serve("cdn.test", func(w http.ResponseWriter, _ *http.Request) {
			if seen.Add(1) == 1 {
				http.Error(w, "upstream deploying", http.StatusServiceUnavailable)
				return
			}
			w.Write(payload)
		})
		p := n.policy("cdn.test")
		p.Retry.sleep = func(context.Context, time.Duration) error { return nil }
		c := n.client(p)

		var got bytes.Buffer
		if err := c.Stream(ctx, "http://cdn.test/jei.jar", 1<<20, func(r io.Reader) error {
			_, err := io.Copy(&got, r)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if seen.Load() != 2 {
			t.Errorf("attempts = %d, want 2", seen.Load())
		}
		if got.Len() != len(payload) {
			t.Errorf("streamed %d bytes", got.Len())
		}
	})

	t.Run("an unbounded stream is refused", func(t *testing.T) {
		n := newTestNet(t)
		n.serve("cdn.test", func(w http.ResponseWriter, _ *http.Request) { w.Write(payload) })
		c := n.client(n.policy("cdn.test"))

		err := c.Stream(ctx, "http://cdn.test/x.jar", 0, func(io.Reader) error { return nil })
		if !errors.Is(err, ErrPolicy) {
			t.Fatalf("Stream without a bound = %v, want ErrPolicy", err)
		}
		if err := c.Stream(ctx, "http://cdn.test/x.jar", 10, nil); !errors.Is(err, ErrPolicy) {
			t.Fatalf("Stream without a sink = %v, want ErrPolicy", err)
		}
	})
}

// TestARefusedStatusIsRetriedOnlyWhenRetryingCouldHelp is the ladder's whole
// value, measured in requests rather than in intentions.
func TestARefusedStatusIsRetriedOnlyWhenRetryingCouldHelp(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		attempts int64
	}{
		// Section 12.8's three classifications. The author opted out, the file
		// was withdrawn, the id was wrong: three messages, three branches, and
		// not one of them changes its mind.
		{"403 the author opted out of API distribution", http.StatusForbidden, 1},
		{"404 a bad id", http.StatusNotFound, 1},
		{"410 the file was withdrawn", http.StatusGone, 1},
		{"451 withdrawn for legal reasons", http.StatusUnavailableForLegalReasons, 1},
		{"400 a malformed request", http.StatusBadRequest, 1},
		{"501 this server does not do that", http.StatusNotImplemented, 1},
		// And the ones that can.
		{"503 a CDN mid-deploy", http.StatusServiceUnavailable, 4},
		{"502 a bad gateway", http.StatusBadGateway, 4},
		{"429 backpressure", http.StatusTooManyRequests, 4},
		{"408 a request timeout", http.StatusRequestTimeout, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newTestNet(t)
			hits := n.serve("api.test", func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"description":"the reason"}`, tc.status)
			})
			p := n.policy("api.test")
			p.Retry.MaxAttempts = 4
			p.Retry.sleep = func(context.Context, time.Duration) error { return nil }
			c := n.client(p)

			_, err := c.Get(context.Background(), "http://api.test/thing")
			if err == nil {
				t.Fatalf("status %d was accepted", tc.status)
			}
			if got := hits.Load(); got != tc.attempts {
				t.Errorf("status %d was requested %d time(s), want %d", tc.status, got, tc.attempts)
			}

			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("error is not a *StatusError: %v", err)
			}
			if se.Code != tc.status {
				t.Errorf("StatusError.Code = %d", se.Code)
			}
			// The API's own error payload is where the actual reason lives; a
			// bare "400 Bad Request" gives the operator nothing to act on.
			if !strings.Contains(se.Snippet, "the reason") {
				t.Errorf("Snippet = %q, want the response's own description", se.Snippet)
			}
			if tc.attempts == 1 && errors.Is(err, ErrExhausted) {
				t.Error("a terminal status was reported as exhausted retries, which reads as " +
					"a gvm failure rather than as the server's answer")
			}
			if tc.attempts > 1 && !errors.Is(err, ErrExhausted) {
				t.Errorf("a transient status gave up without saying so: %v", err)
			}
		})
	}
}

func TestATransientStatusFollowedBySuccessSucceeds(t *testing.T) {
	n := newTestNet(t)
	var seen atomic.Int64
	n.serve("api.test", func(w http.ResponseWriter, _ *http.Request) {
		if seen.Add(1) < 3 {
			http.Error(w, "try later", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	})
	p := n.policy("api.test")
	p.Retry.sleep = func(context.Context, time.Duration) error { return nil }
	c := n.client(p)

	resp, err := c.Get(context.Background(), "http://api.test/thing")
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Errorf("body = %q", resp.Body)
	}
	if seen.Load() != 3 {
		t.Errorf("attempts = %d, want 3", seen.Load())
	}
}

// TestA429PausesTheWholeLimiterNotJustThisRequest: the limit being enforced is
// per-IP, so the other goroutines in a fan-out sync are the same offender.
// Backing off only the failing request is taking turns being refused.
func TestA429PausesTheWholeLimiterNotJustThisRequest(t *testing.T) {
	n := newTestNet(t)
	n.serve("api.test", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	})
	p := n.policy("api.test")
	p.Retry.MaxAttempts = 1
	c := n.client(p)

	_, err := c.Get(context.Background(), "http://api.test/thing")
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v", err)
	}
	if se.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s", se.RetryAfter)
	}
	if paused := c.Limiter().Paused(); paused < 6*time.Second {
		t.Errorf("the limiter is paused for %v; the server's Retry-After must apply to every "+
			"caller of it, not just the request that earned it", paused)
	}
}

func TestAnExhaustedPublishedBudgetPausesBeforeItIsRefused(t *testing.T) {
	// Modrinth publishes its 300-per-minute limit in headers. A client that
	// ignored them would discover the limit by being refused.
	n := newTestNet(t)
	n.serve("api.test", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Reset", "9")
		fmt.Fprint(w, `{"ok":true}`)
	})
	c := n.client(n.policy("api.test"))

	if _, err := c.Get(context.Background(), "http://api.test/v2/project"); err != nil {
		t.Fatal(err)
	}
	if paused := c.Limiter().Paused(); paused < 8*time.Second {
		t.Errorf("the limiter is paused for %v after a published budget hit zero", paused)
	}
}

func TestAContextCancellationStopsTheLadder(t *testing.T) {
	n := newTestNet(t)
	hits := n.serve("api.test", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "later", http.StatusServiceUnavailable)
	})
	ctx, cancel := context.WithCancel(context.Background())
	p := n.policy("api.test")
	p.Retry.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel() // the operator's ctrl-C, landing during a backoff
		return ctx.Err()
	}
	c := n.client(p)

	_, err := c.Get(ctx, "http://api.test/thing")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("the ladder kept going after cancellation: %d requests", got)
	}
	// The cause survives the cancellation, because "cancelled" alone does not
	// tell an operator what was going wrong when they gave up.
	var se *StatusError
	if !errors.As(err, &se) {
		t.Errorf("the cancellation lost the underlying failure: %v", err)
	}
}

func TestACredentialBearingURLIsRefusedBeforeAnyRequest(t *testing.T) {
	n := newTestNet(t)
	hits := n.serve("api.test", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "{}") })
	c := n.client(n.policy("api.test"))

	_, err := c.Get(context.Background(), "http://user:hunter2@api.test/thing")
	if !errors.Is(err, ErrCredentialsInURL) {
		t.Fatalf("err = %v, want ErrCredentialsInURL", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the refusal printed the password: %v", err)
	}
	if hits.Load() != 0 {
		t.Error("a request was sent")
	}
}

func TestASignedURLIsRedactedToItsPath(t *testing.T) {
	// Section 12.4: "CF's signed URLs are redacted to their path before being
	// recorded." The only way to honour that everywhere is for the query never
	// to reach a string a caller might log.
	n := newTestNet(t)
	n.serve("api.test", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	c := n.client(n.policy("api.test"))

	_, err := c.Get(context.Background(), "http://api.test/files/5346281?token=SECRETVALUE&exp=1")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), "SECRETVALUE") {
		t.Fatalf("the error carries the signed URL's token: %v", err)
	}
	if !strings.Contains(err.Error(), "/files/5346281") {
		t.Errorf("the error does not name the path: %v", err)
	}

	resp, err := c.Get(context.Background(), "http://api.test/ok")
	if err == nil && strings.Contains(resp.URL, "token") {
		t.Errorf("Response.URL carries a query: %q", resp.URL)
	}
}

func TestNewRefusesAnUnenforceablePolicy(t *testing.T) {
	good := SteamPolicy()
	cases := []struct {
		name  string
		mutor func(*Policy)
	}{
		{"no hosts", func(p *Policy) { p.Hosts = nil }},
		{"a wildcard host", func(p *Policy) { p.Hosts = []string{"*.steampowered.com"} }},
		{"a suffix pattern", func(p *Policy) { p.Hosts = []string{".steampowered.com"} }},
		{"a URL instead of a host", func(p *Policy) { p.Hosts = []string{"https://api.steampowered.com"} }},
		{"no user agent", func(p *Policy) { p.UserAgent = "" }},
		{"an unbounded body", func(p *Policy) { p.MaxResponseBytes = 0 }},
		{"no timeout", func(p *Policy) { p.Timeout = 0 }},
		{"an unbounded redirect chain", func(p *Policy) { p.MaxRedirects = 99 }},
		{"no rate limit", func(p *Policy) { p.Rate = 0 }},
		{"a burst of zero", func(p *Policy) { p.Burst = 0 }},
		{"no attempts", func(p *Policy) { p.Retry.MaxAttempts = 0 }},
		{"a busy-loop backoff", func(p *Policy) { p.Retry.Base = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := good
			p.Hosts = append([]string(nil), good.Hosts...)
			tc.mutor(&p)
			if _, err := New(p); !errors.Is(err, ErrPolicy) {
				t.Fatalf("New(%s) = %v, want ErrPolicy", tc.name, err)
			}
		})
	}
	if _, err := New(good); err != nil {
		t.Fatalf("the control policy was refused: %v", err)
	}
}
