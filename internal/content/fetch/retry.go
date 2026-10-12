package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrExhausted is a ladder that ran out of attempts. The cause is wrapped
// alongside it, so a caller can match on either.
var ErrExhausted = errors.New("fetch: retries exhausted")

// errNoRetry marks an error that is terminal for a reason about gvm's own state
// rather than about the server. See Client.Stream: once a sink has consumed
// bytes, a retry would feed it a second copy from the beginning.
var errNoRetry = errors.New("fetch: not retryable from here")

// Class is what a ladder needs to know about a failure: try again, or stop.
//
// The distinction is the whole value of the ladder. A 403 from CurseForge means
// the author opted out of API distribution (section 12.8) and it will mean that
// on every subsequent attempt; retrying it four times with backoff turns a
// clear, actionable report into twelve seconds of silence followed by the same
// report. Conversely a 503 during a CDN deploy is gone in two seconds.
type Class int

const (
	// ClassOK is not a failure.
	ClassOK Class = iota
	// ClassTransient may succeed if asked again.
	ClassTransient
	// ClassTerminal will not. Asking again is a loop with a delay in it.
	ClassTerminal
)

func (c Class) String() string {
	switch c {
	case ClassOK:
		return "ok"
	case ClassTransient:
		return "transient"
	case ClassTerminal:
		return "terminal"
	}
	return fmt.Sprintf("class(%d)", int(c))
}

// StatusError is an HTTP status gvm would not accept.
//
// It carries the body's first bytes because an API's error payload is where the
// actual reason lives -- Modrinth and CurseForge both answer 400 with a JSON
// "description" that names the malformed parameter, and an operator reading
// "400 Bad Request" alone has nothing to act on.
type StatusError struct {
	Code       int
	Status     string
	URL        string
	Method     string
	RetryAfter time.Duration
	Snippet    string
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("fetch: %s %s: %s", e.Method, e.URL, e.Status)
	if e.RetryAfter > 0 {
		s += fmt.Sprintf(" (Retry-After %s)", e.RetryAfter)
	}
	if e.Snippet != "" {
		s += ": " + e.Snippet
	}
	return s
}

// Class classifies this status.
func (e *StatusError) Class() Class { return ClassifyStatus(e.Code) }

// Blocked reports the CurseForge opt-out shape: a 403 on a download URL means
// the author withheld API distribution, which is a report to the operator and
// not an error to retry (section 12.8, rung 6).
func (e *StatusError) Blocked() bool {
	return e != nil && (e.Code == http.StatusForbidden || e.Code == http.StatusUnavailableForLegalReasons)
}

// ClassifyStatus maps an HTTP status onto the ladder's decision.
//
// The 4xx list is spelled out rather than reduced to "4xx is terminal" because
// four of them are not: 408 and 425 are timing, 429 is backpressure, and 409
// can be a transient conflict. The 5xx list is spelled out for the opposite
// reason: 501 is a permanent "this server does not do that".
func ClassifyStatus(code int) Class {
	switch {
	case code >= 200 && code < 300:
		return ClassOK
	case code == http.StatusNotModified:
		// A conditional request answered "unchanged". Not a failure, and the
		// caller is expected to look at the code.
		return ClassOK
	}
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusTooEarly,            // 425
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
		http.StatusInsufficientStorage, // 507
		509:                            // Bandwidth Limit Exceeded, non-standard but real
		return ClassTransient
	case http.StatusNotImplemented: // 501
		return ClassTerminal
	}
	switch {
	case code >= 300 && code < 400:
		// A redirect that reached a caller rather than being followed is one
		// the policy refused or one the chain bound. Either way the answer
		// does not change.
		return ClassTerminal
	case code >= 400 && code < 500:
		// Including 403 and 451, which are section 12.8's opt-out and legal
		// withdrawal: terminal, and both have a human-shaped next step that a
		// retry loop only delays.
		return ClassTerminal
	case code >= 500:
		return ClassTransient
	}
	return ClassTerminal
}

// Classify decides whether err is worth asking again about.
//
// The unknown case is TRANSIENT, deliberately. An error nobody has named is
// more likely to be a network condition than a permanent refusal, a retry costs
// seconds, and the alternative -- giving up on a transient failure because it
// was not on a list -- fails a nightly sync with a message nobody can act on.
// Everything with a human-shaped next step is named above.
func Classify(err error) Class {
	if err == nil {
		return ClassOK
	}

	// gvm's own refusals first: no amount of retrying moves an allowlist.
	switch {
	case errors.Is(err, ErrHostNotAllowed),
		errors.Is(err, ErrScheme),
		errors.Is(err, ErrCredentialsInURL),
		errors.Is(err, ErrRedirect),
		errors.Is(err, ErrTooLarge),
		errors.Is(err, ErrPolicy),
		errors.Is(err, ErrNoSource),
		errors.Is(err, errNoRetry):
		return ClassTerminal
	// A cancelled or expired context is the operator's decision or the job's
	// budget. Retrying spends a budget that is already gone.
	case errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return ClassTerminal
	}

	var se *StatusError
	if errors.As(err, &se) {
		return se.Class()
	}

	// A certificate that does not verify will not verify in two seconds, and
	// retrying one is how a man-in-the-middle gets four chances.
	var cve *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	switch {
	case errors.As(err, &cve),
		errors.As(err, &unknownAuthority),
		errors.As(err, &hostnameErr),
		errors.As(err, &invalidCert):
		return ClassTerminal
	}

	// NXDOMAIN is terminal; SERVFAIL and a resolver timeout are not. The
	// difference matters on a box that is still bringing up its network.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return ClassTerminal
		}
		return ClassTransient
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ClassTransient
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return ClassTransient
	}
	return ClassTransient
}

// Retry is a bounded ladder: a finite number of attempts, exponential backoff
// with full jitter, and an overall budget.
//
// Bounded in BOTH dimensions on purpose. MaxAttempts alone would let four
// attempts against a server that hangs for its full timeout each time consume
// two minutes inside a download cycle with the game stopped; Total alone would
// let a fast-failing server be hammered hundreds of times.
type Retry struct {
	// MaxAttempts counts the first try. 1 means "no retries".
	MaxAttempts int
	// Base is the first backoff; each subsequent one doubles, capped at Max.
	Base time.Duration
	Max  time.Duration
	// Total bounds the whole ladder including the attempts themselves. Zero
	// means "no overall bound", which is only safe because MaxAttempts is
	// finite.
	Total time.Duration

	// jitter and sleep are this package's test seams. Backoff with real
	// jitter is untestable without them, and a test that slept for the real
	// durations is a test somebody disables.
	jitter func(time.Duration) time.Duration
	sleep  func(context.Context, time.Duration) error
}

// DefaultRetry is four attempts over at most two minutes.
//
// Four, not ten: these calls sit inside the download cycle of section 12.6,
// where steps 3 to 9 run with the server STOPPED. A ladder that is generous
// with time is a ladder that is generous with downtime. A transient CDN failure
// clears inside four attempts or it is not transient.
func DefaultRetry() Retry {
	return Retry{
		MaxAttempts: 4,
		Base:        500 * time.Millisecond,
		Max:         8 * time.Second,
		Total:       2 * time.Minute,
	}
}

// Validate refuses a ladder that cannot terminate.
func (r Retry) Validate() error {
	switch {
	case r.MaxAttempts < 1:
		return fmt.Errorf("%w: MaxAttempts is %d, so nothing would ever be attempted",
			ErrPolicy, r.MaxAttempts)
	case r.MaxAttempts > 10:
		return fmt.Errorf("%w: MaxAttempts is %d; these calls run with the server stopped, so "+
			"a generous ladder is generous with downtime", ErrPolicy, r.MaxAttempts)
	case r.Base <= 0 && r.MaxAttempts > 1:
		return fmt.Errorf("%w: Base backoff is %v, so retries would be a busy loop",
			ErrPolicy, r.Base)
	case r.Max < r.Base:
		return fmt.Errorf("%w: Max backoff %v is below Base %v", ErrPolicy, r.Max, r.Base)
	case r.Total < 0:
		return fmt.Errorf("%w: Total is %v", ErrPolicy, r.Total)
	}
	return nil
}

// Do runs fn until it succeeds, until the failure is terminal, or until the
// ladder runs out.
//
// fn takes the attempt number because the thing it is retrying is usually worth
// logging with it, and because a caller that wants to vary behaviour by attempt
// (a smaller batch, a different rung of section 12.8's ladder) should not have
// to close over a counter.
func Do(ctx context.Context, r Retry, fn func(ctx context.Context, attempt int) error) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	jitter := r.jitter
	if jitter == nil {
		jitter = fullJitter
	}
	sleep := r.sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	start := time.Now()
	var last error
	for attempt := 1; attempt <= r.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return fmt.Errorf("%w after %d attempt(s): %w", err, attempt-1, last)
			}
			return err
		}

		err := fn(ctx, attempt)
		if err == nil {
			return nil
		}
		last = err

		if Classify(err) == ClassTerminal {
			// Returned with no wrapping and no delay. The caller's next move
			// is to report this to a human, and a "gave up after 1 attempt"
			// prefix on a 403 would read as a gvm failure rather than as the
			// author's opt-out that it is.
			return err
		}
		if attempt == r.MaxAttempts {
			break
		}

		wait := backoff(r, attempt, jitter)
		if after := retryAfter(err); after > wait {
			// The server named a number. Honouring it is both politeness and
			// self-interest: ignoring a Retry-After is how a keyless endpoint
			// stops answering this IP at all.
			wait = after
		}
		if r.Total > 0 && time.Since(start)+wait >= r.Total {
			return fmt.Errorf("%w: the %v budget is spent after %d attempt(s) and the next "+
				"backoff is %v: %w", ErrExhausted, r.Total, attempt, wait, err)
		}
		if err := sleep(ctx, wait); err != nil {
			return fmt.Errorf("%w after %d attempt(s): %w", err, attempt, last)
		}
	}
	return fmt.Errorf("%w after %d attempt(s): %w", ErrExhausted, r.MaxAttempts, last)
}

// backoff is Base * 2^(attempt-1), capped at Max, then jittered.
func backoff(r Retry, attempt int, jitter func(time.Duration) time.Duration) time.Duration {
	d := r.Base
	for range attempt - 1 {
		d *= 2
		if d >= r.Max {
			d = r.Max
			break
		}
	}
	return jitter(d)
}

// fullJitter spreads a backoff over [d/2, d).
//
// Full jitter rather than a fixed delay because gvm runs N instances on one box
// off one nightly timer: without jitter, twenty instances that all failed on
// the same CDN blip would retry in the same millisecond, forever, which is the
// thundering herd that made the blip.
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// retryAfter is the server's own number, if it gave one.
func retryAfter(err error) time.Duration {
	var se *StatusError
	if errors.As(err, &se) {
		return se.RetryAfter
	}
	return 0
}

// ParseRetryAfter decodes a Retry-After header, in both of its forms.
//
// RFC 9110 allows delta-seconds and an HTTP-date, and real services send both:
// Steam sends seconds, CDNs sometimes send a date. A parser that handled only
// one would silently ignore the other, which looks exactly like a server that
// sent no header -- so gvm would retry straight into the next refusal.
//
// A date in the past yields zero rather than a negative wait, and an
// absurdly long value is capped: a Retry-After of 24 hours inside a download
// cycle is not a wait, it is a hang, and the right response is to fail the job
// and let the operator see the number.
func ParseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs <= 0 {
			return 0, true
		}
		return capRetryAfter(time.Duration(secs * float64(time.Second))), true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d <= 0 {
			return 0, true
		}
		return capRetryAfter(d), true
	}
	return 0, false
}

// maxRetryAfter is the longest server-requested pause gvm will sit through.
const maxRetryAfter = 60 * time.Second

func capRetryAfter(d time.Duration) time.Duration {
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}
