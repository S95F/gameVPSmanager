// Package fetch is gvm's outbound HTTP, and the only part of gvm that talks to
// the internet (D-16, section 12.4).
//
// It runs as `gvm-fetch` -- neither root nor the instance user -- in a transient
// systemd service, in no group that can reach admin.sock, with no Postgres
// grant. That is the containment for the bytes it brings back. This package is
// the containment for where it may go:
//
//  1. A HARD per-source hostname allowlist, enforced in THREE places: on the
//     URL before the request, on every redirect, and in DialContext. The three
//     are not redundant. The first is policy, the second is the one that
//     matters -- A REDIRECT OFF THE ALLOWLIST IS AN ERROR, NOT A FOLLOW,
//     because an allowlist a hostile redirect can step around is not an
//     allowlist -- and the third still holds if some future caller reaches for
//     the transport directly.
//
//  2. BOUNDED BODIES. Every response is read through a limit, and the limit is
//     applied to the DECODED stream, which is the only place it defends against
//     a gzip bomb. An unbounded io.ReadAll of a hostile response is an
//     out-of-memory kill on a 2 GB VPS, and there is no spelling of a Policy
//     that means "no limit".
//
//  3. A BOUNDED, CLASSIFYING RETRY LADDER. Terminal failures are returned
//     immediately: a 403 from CurseForge is the author's distribution opt-out
//     and it will say the same thing four times (section 12.8). These calls sit
//     inside the download cycle of section 12.6, where the server is STOPPED, so
//     a ladder that is generous with time is generous with downtime.
//
// What it is NOT: it holds no credentials and no blob store. Tokens arrive as
// headers the caller sets from LoadCredential; bytes leave through a sink the
// caller supplies, which is how the digest check stays in internal/provider's
// store and out of the thing that chose the URL.
package fetch

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/S95F/gameVPSmanager/internal/version"
)

// rateLimitPause is how long a 429 with no Retry-After header pauses the whole
// limiter. The server said slow down without saying how much; five seconds is
// long enough to matter and short enough to survive inside a download cycle.
const rateLimitPause = 5 * time.Second

// snippetBytes is how much of an error response body is kept for the message.
// An API's error payload names the malformed parameter in its first line;
// keeping the whole body would put a megabyte of HTML in a log.
const snippetBytes = 240

// Client is one content source's HTTP client.
//
// One per source, because the allowlist, the rate limit and the retry ladder
// are all per-source, and a shared client would mean Modrinth's 300/min budget
// being spent by a Steam collection sync.
type Client struct {
	p   Policy
	hc  *http.Client
	lim *Limiter
	ua  string
}

// New builds a client from a validated policy.
func New(p Policy) (*Client, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	c := &Client{
		p:   p,
		lim: NewLimiter(p.Rate, p.Burst),
		ua:  p.UserAgent + "/" + version.Version,
	}
	c.hc = &http.Client{
		Transport:     c.transport(),
		CheckRedirect: c.checkRedirect,
		// No client-level Timeout. It would cut a 2 GB modpack download at a
		// number chosen for an API call; the per-attempt bound that belongs
		// here is ResponseHeaderTimeout, and a download's overall budget is
		// the job's context.
	}
	return c, nil
}

// Policy is the contract this client enforces, for a doctor line.
func (c *Client) Policy() Policy { return c.p }

// Limiter is the shared token bucket. Exposed so a caller that fans out over
// several clients for one source can share one budget, and so a progress line
// can say "paused 4s at the server's request" instead of looking hung.
func (c *Client) Limiter() *Limiter { return c.lim }

// transport is the layer the allowlist's last line lives in.
func (c *Client) transport() *http.Transport {
	return &http.Transport{
		// NO PROXY, and not by omission. http.ProxyFromEnvironment would make
		// the transport dial the proxy instead of the target, which both
		// defeats the DialContext allowlist (it would be judging the proxy's
		// hostname) and is then refused by it, since no proxy is on any
		// source's list. Failing closed is correct, but it would fail closed
		// with a baffling message on any box that happens to have http_proxy
		// set, so the decision is explicit and documented here instead.
		Proxy:       nil,
		DialContext: c.dialAllowed,
		// TLS 1.2 floor. All three APIs have been 1.2+ for years and the
		// bytes behind them get executed as a service account.
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		// The per-attempt bound. A server that accepts a connection and never
		// answers is otherwise a hang, and a hang in step 5 of the download
		// cycle is a server that stays stopped.
		ResponseHeaderTimeout: c.p.Timeout,
	}
}

// dialAllowed is the third allowlist layer: no connection to an off-list host
// is ever opened, whatever asked for it.
func (c *Client) dialAllowed(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not host:port", ErrPolicy, addr)
	}
	if !c.p.AllowHost(host) {
		// The address AS GIVEN, quoted: this line is an audit record, and
		// printing the normalised form named the host gvm was being made to
		// imitate rather than the one it was asked to dial.
		return nil, fmt.Errorf("%w: refused to dial %q; the allowlist is enforced at the "+
			"socket as well as on the URL, so a code path that skipped the URL check still "+
			"cannot reach it (allowed: %s)",
			ErrHostNotAllowed, addr, strings.Join(c.p.Hosts, ", "))
	}
	if c.p.dial != nil {
		return c.p.dial(ctx, network, addr)
	}
	d := &net.Dialer{Timeout: c.p.Timeout, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// checkRedirect is the second allowlist layer, and the one this package exists
// for.
//
// A REDIRECT TO AN OFF-ALLOWLIST HOST IS AN ERROR, NOT A FOLLOW. The whole
// point of a hostname allowlist is that an attacker who can influence a
// response -- a compromised CDN, a cache poisoner, a hijacked project page --
// cannot use it to make gvm fetch mod bytes from somewhere else. A client that
// checked only the first URL and then followed wherever it was sent would have
// an allowlist that any 302 defeats.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	from := "the request"
	if len(via) > 0 {
		from = redact(via[len(via)-1].URL)
	}
	if len(via) > c.p.MaxRedirects {
		return fmt.Errorf("%w: %s led to more than %d redirects; a chain that long is a loop "+
			"somebody has not noticed", ErrRedirect, from, c.p.MaxRedirects)
	}
	if err := c.p.checkScheme(req.URL); err != nil {
		return fmt.Errorf("%w: %s redirected to %s: %w", ErrRedirect, from,
			redact(req.URL), err)
	}
	if !c.p.AllowHost(req.URL.Host) {
		// The host AS GIVEN, quoted, for the same reason as dialAllowed's.
		return fmt.Errorf("%w: %s redirected to %q, which is not on this source's allowlist "+
			"(%s); gvm does not follow a redirect off its own allowlist, because an "+
			"allowlist a 302 can step around is not an allowlist: %w",
			ErrRedirect, from, req.URL.Host, strings.Join(c.p.Hosts, ", "),
			ErrHostNotAllowed)
	}
	return nil
}

// Response is a bodied response that fitted inside its bound.
type Response struct {
	// URL is redacted: scheme, host and path, with any query dropped. Section
	// 12.4 requires a signed URL to be recorded as its path, and the only way
	// to honour that everywhere is for the query never to be in the value a
	// caller might log.
	URL    string
	Status int
	Header http.Header
	Body   []byte
}

// Get fetches a bounded body.
func (c *Client) Get(ctx context.Context, raw string) (*Response, error) {
	return c.bodied(ctx, http.MethodGet, raw, nil)
}

// PostForm posts a urlencoded form and fetches a bounded body.
//
// Steam's ISteamRemoteStorage/GetCollectionDetails is POST-only with a form
// body, so this is not an optional convenience: it is how a collection sync
// happens at all.
func (c *Client) PostForm(ctx context.Context, raw string, form url.Values) (*Response, error) {
	return c.bodied(ctx, http.MethodPost, raw, form)
}

// JSON fetches and decodes.
//
// DisallowUnknownFields is NOT set. A content API that adds a field must not
// break a sync; the fields gvm depends on are cross-checked by the caller
// instead, which is section 12.5's rule that every child must be accounted for.
func (c *Client) JSON(ctx context.Context, raw string, out any) error {
	resp, err := c.Get(ctx, raw)
	if err != nil {
		return err
	}
	return decode(resp, out)
}

// PostFormJSON posts a form and decodes the answer.
func (c *Client) PostFormJSON(ctx context.Context, raw string, form url.Values, out any) error {
	resp, err := c.PostForm(ctx, raw, form)
	if err != nil {
		return err
	}
	return decode(resp, out)
}

func decode(resp *Response, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("fetch: %s returned %d bytes that are not the expected JSON: %w",
			resp.URL, len(resp.Body), err)
	}
	return nil
}

// bodied is the retry ladder wrapped around one bounded request.
func (c *Client) bodied(ctx context.Context, method, raw string, form url.Values) (*Response, error) {
	u, err := c.p.CheckURL(raw)
	if err != nil {
		return nil, err
	}
	var out *Response
	err = Do(ctx, c.p.Retry, func(ctx context.Context, _ int) error {
		resp, err := c.attempt(ctx, method, u, form)
		if err != nil {
			return err
		}
		out = resp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// attempt is one request, one bounded read, one classification.
func (c *Client) attempt(ctx context.Context, method string, u *url.URL, form url.Values) (*Response, error) {
	if err := c.lim.Wait(ctx); err != nil {
		return nil, err
	}

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPolicy, err)
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json, text/html;q=0.8, */*;q=0.1")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		// On a CheckRedirect refusal net/http returns the previous response
		// alongside the error, with its body already closed. Closing it again
		// is harmless and means this branch does not depend on that promise.
		closeBody(resp)
		return nil, c.wrap(method, u, err)
	}
	defer resp.Body.Close()

	// The declared length is checked before a byte is read, so a hostile
	// 10 GB Content-Length costs one round trip rather than 10 GB of
	// bandwidth. It is a HINT and not the bound: the header can lie or be
	// absent, and chunked responses have none, which is why the reader below
	// is bounded regardless.
	if resp.ContentLength > c.p.MaxResponseBytes {
		return nil, fmt.Errorf("%w: %s declared %d bytes and the bound is %d",
			ErrTooLarge, redact(u), resp.ContentLength, c.p.MaxResponseBytes)
	}

	data, readErr := readBounded(resp.Body, c.p.MaxResponseBytes)

	// The status is classified BEFORE the read error is returned. A 503 whose
	// body was truncated is a 503 -- transient, retryable -- and reporting it
	// as a read failure would hide the one fact the ladder needs.
	if ClassifyStatus(resp.StatusCode) != ClassOK {
		return nil, c.statusError(method, u, resp, data)
	}
	if readErr != nil {
		return nil, readErr
	}
	c.observeRateHeaders(resp.Header)

	return &Response{
		URL:    redact(u),
		Status: resp.StatusCode,
		Header: resp.Header,
		Body:   data,
	}, nil
}

// Stream fetches a large body through a sink, bounded by max.
//
// max is REQUIRED. Defaulting it to the policy's response bound would refuse
// every real mod jar, and defaulting it to unlimited is the thing this package
// is for; a download's bound is the size the plan declared, plus whatever slack
// the caller thinks is honest.
//
// The sink is the blob store's Put. That is the division of labour: this
// package decides where the bytes may come from and how many there may be, and
// the store decides whether they are the approved bytes. Neither can be talked
// into the other's job.
//
// A failure AFTER the sink has started reading is terminal, even if it would
// otherwise be transient. A retry would hand the sink a second stream starting
// from byte zero, and a sink that is hashing bytes into a digest would then be
// hashing a concatenation of two attempts -- which fails the digest check and
// reports a tampered download. The caller retries a stream by calling Stream
// again with a fresh sink.
func (c *Client) Stream(ctx context.Context, raw string, max int64, sink func(io.Reader) error) error {
	if max <= 0 {
		return fmt.Errorf("%w: Stream needs an explicit byte bound; an unbounded download is "+
			"an out-of-memory kill or a full disk, and the bound a caller has is the size the "+
			"plan declared", ErrPolicy)
	}
	if sink == nil {
		return fmt.Errorf("%w: Stream needs a sink", ErrPolicy)
	}
	u, err := c.p.CheckURL(raw)
	if err != nil {
		return err
	}

	return Do(ctx, c.p.Retry, func(ctx context.Context, _ int) error {
		return c.streamOnce(ctx, u, max, sink)
	})
}

func (c *Client) streamOnce(ctx context.Context, u *url.URL, max int64, sink func(io.Reader) error) error {
	if err := c.lim.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPolicy, err)
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "*/*")

	resp, err := c.hc.Do(req)
	if err != nil {
		closeBody(resp)
		return c.wrap(http.MethodGet, u, err)
	}
	defer resp.Body.Close()

	if ClassifyStatus(resp.StatusCode) != ClassOK {
		// The error body is small and worth keeping: a CurseForge 403 answers
		// with the reason, and that reason is what the blocked-mods report
		// shows the operator.
		data, _ := readBounded(resp.Body, snippetBytes*4)
		return c.statusError(http.MethodGet, u, resp, data)
	}
	if resp.ContentLength > max {
		return fmt.Errorf("%w: %s declared %d bytes and the plan allowed %d",
			ErrTooLarge, redact(u), resp.ContentLength, max)
	}
	c.observeRateHeaders(resp.Header)

	if err := sink(&boundedReader{r: resp.Body, left: max, what: redact(u), max: max}); err != nil {
		// Marked not-retryable here rather than classified at the top, because
		// the reason is about gvm's state -- the sink has consumed bytes --
		// and not about the error itself.
		return fmt.Errorf("%w: %s: %w", errNoRetry, redact(u), err)
	}
	return nil
}

// statusError builds the classified status failure, and tells the limiter about
// a server-requested pause.
func (c *Client) statusError(method string, u *url.URL, resp *http.Response, body []byte) error {
	se := &StatusError{
		Code:    resp.StatusCode,
		Status:  resp.Status,
		URL:     redact(u),
		Method:  method,
		Snippet: snippet(body),
	}
	if d, ok := ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
		se.RetryAfter = d
	} else if resp.StatusCode == http.StatusTooManyRequests {
		// The server said slow down without saying how much. Guessing short
		// is how the next request earns the same answer.
		se.RetryAfter = rateLimitPause
	}
	// Applied to the LIMITER, not just to this request's backoff: the limit
	// being enforced is per-IP, so the other goroutines in this sync are the
	// same offender.
	c.lim.PauseFor(se.RetryAfter)
	return se
}

// observeRateHeaders honours a published budget before it runs out.
//
// Modrinth publishes its 300-per-minute limit in headers (section 12.8), and a
// client that ignored them would discover the limit by being refused. The
// header names are the de-facto ones; a service that sends neither simply gets
// the token bucket.
func (c *Client) observeRateHeaders(h http.Header) {
	rem := strings.TrimSpace(h.Get("X-Ratelimit-Remaining"))
	if rem == "" {
		return
	}
	n, err := strconv.Atoi(rem)
	if err != nil || n > 0 {
		return
	}
	reset := strings.TrimSpace(h.Get("X-Ratelimit-Reset"))
	if reset == "" {
		c.lim.PauseFor(rateLimitPause)
		return
	}
	if secs, err := strconv.Atoi(reset); err == nil && secs > 0 {
		c.lim.PauseFor(capRetryAfter(time.Duration(secs) * time.Second))
		return
	}
	// A reset expressed as an absolute epoch second, which some services send.
	if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
		if d := time.Until(time.Unix(epoch, 0)); d > 0 {
			c.lim.PauseFor(capRetryAfter(d))
		}
	}
}

// wrap turns a transport error into one gvm can classify and log.
//
// The *url.Error wrapper is unwrapped first so the message carries the REDACTED
// URL rather than the full one: a CurseForge signed download URL has the token
// in its query, and net/http's own error text would put it in the job log.
// errors.Is and errors.As still see the inner error, which is what Classify
// matches on.
func (c *Client) wrap(method string, u *url.URL, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	return fmt.Errorf("fetch: %s %s: %w", method, redact(u), err)
}

func closeBody(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

// redact is the URL a message or a lock may carry: scheme, host and path, with
// the query dropped.
//
// Section 12.4: "CF's signed URLs are redacted to their path before being
// recorded." The only way to honour that everywhere is for the unredacted form
// never to reach a string a caller might print, so this is what every error and
// every Response carries.
func redact(u *url.URL) string {
	if u == nil {
		return "<no url>"
	}
	out := u.Scheme + "://" + u.Host + u.Path
	if u.RawQuery != "" {
		out += "?<redacted>"
	}
	return out
}

// readBounded reads at most max bytes and refuses a longer body.
//
// The bound is applied here, to the body net/http hands over, which is the
// DECODED stream: a gzip bomb that expands to 10 GB is stopped at max, whereas a
// bound on the compressed length would let it through.
//
// It reads max+1 and compares, rather than trusting io.ReadAll of a LimitReader:
// a LimitReader that stops exactly at max returns a SHORT BODY WITH NO ERROR,
// and a truncated JSON document that happens to parse is the worst available
// outcome.
func readBounded(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%w: the response exceeds %d bytes", ErrTooLarge, max)
	}
	if err != nil {
		return data, fmt.Errorf("fetch: read response: %w", err)
	}
	return data, nil
}

// boundedReader is readBounded for a stream nobody buffers.
//
// It errors at max+1 rather than reporting EOF, for the same reason: a sink
// that was handed a silently truncated jar would hash it, fail the digest, and
// report a tampered download for what is actually a bound being hit.
type boundedReader struct {
	r    io.Reader
	left int64
	max  int64
	what string
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.left < 0 {
		return 0, fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, b.what, b.max)
	}
	// One byte of headroom, so the overrun is observed rather than inferred
	// from a short read.
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return n, fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, b.what, b.max)
	}
	return n, err
}

// snippet keeps the first useful line of an error body.
func snippet(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	s := string(body)
	if len(s) > snippetBytes {
		s = s[:snippetBytes] + "…"
	}
	// Collapsed to one line and stripped of control characters: this goes into
	// a log line and an API that answers with HTML would otherwise paste a
	// page into it.
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
