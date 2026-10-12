package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestClassifyStatusTellsTheTwoFailuresApart. The ladder's entire value is in
// this table: retrying something that will keep saying no is a loop with a delay
// in it, and giving up on a CDN mid-deploy fails a nightly sync for nothing.
func TestClassifyStatusTellsTheTwoFailuresApart(t *testing.T) {
	cases := []struct {
		code int
		want Class
		why  string
	}{
		{http.StatusOK, ClassOK, ""},
		{http.StatusNoContent, ClassOK, ""},
		{http.StatusNotModified, ClassOK, "a conditional request answered \"unchanged\""},

		{http.StatusMovedPermanently, ClassTerminal, "a redirect that reached a caller is one the policy refused"},
		{http.StatusFound, ClassTerminal, ""},
		{http.StatusBadRequest, ClassTerminal, "Modrinth's silently-dropped filters land here"},
		{http.StatusUnauthorized, ClassTerminal, "a missing or wrong token"},
		{http.StatusForbidden, ClassTerminal, "CurseForge's distribution opt-out (section 12.8)"},
		{http.StatusNotFound, ClassTerminal, "a bad id"},
		{http.StatusGone, ClassTerminal, "the file was withdrawn: terminal, and manual download fails too"},
		{http.StatusUnprocessableEntity, ClassTerminal, ""},
		{http.StatusUnavailableForLegalReasons, ClassTerminal, "withdrawn: a human-shaped next step"},
		{http.StatusNotImplemented, ClassTerminal, "a permanent \"this server does not do that\""},

		{http.StatusRequestTimeout, ClassTransient, "timing, not refusal"},
		{http.StatusTooEarly, ClassTransient, ""},
		{http.StatusTooManyRequests, ClassTransient, "backpressure: the one 4xx that asks to be asked again"},
		{http.StatusInternalServerError, ClassTransient, ""},
		{http.StatusBadGateway, ClassTransient, ""},
		{http.StatusServiceUnavailable, ClassTransient, "a CDN mid-deploy"},
		{http.StatusGatewayTimeout, ClassTransient, ""},
		{http.StatusInsufficientStorage, ClassTransient, ""},
		{509, ClassTransient, "Bandwidth Limit Exceeded: non-standard and real"},
	}
	for _, tc := range cases {
		if got := ClassifyStatus(tc.code); got != tc.want {
			t.Errorf("ClassifyStatus(%d) = %v, want %v %s", tc.code, got, tc.want, tc.why)
		}
	}
}

type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "i/o timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

func TestClassifyNeverRetriesAPolicyDecisionOrACertificate(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Class
	}{
		// gvm's own refusals. No amount of asking moves an allowlist, and a
		// ladder that retried one would turn a policy decision into a loop.
		{"off the allowlist", fmt.Errorf("x: %w", ErrHostNotAllowed), ClassTerminal},
		{"plain HTTP", fmt.Errorf("x: %w", ErrScheme), ClassTerminal},
		{"credentials in the URL", fmt.Errorf("x: %w", ErrCredentialsInURL), ClassTerminal},
		{"a refused redirect", fmt.Errorf("x: %w", ErrRedirect), ClassTerminal},
		{"an oversized body", fmt.Errorf("x: %w", ErrTooLarge), ClassTerminal},
		{"a malformed policy", fmt.Errorf("x: %w", ErrPolicy), ClassTerminal},
		{"an unknown source", fmt.Errorf("x: %w", ErrNoSource), ClassTerminal},
		{"a sink that already consumed bytes", fmt.Errorf("x: %w", errNoRetry), ClassTerminal},

		// The operator's decision, and the job's spent budget.
		{"cancelled", context.Canceled, ClassTerminal},
		{"deadline exceeded", context.DeadlineExceeded, ClassTerminal},

		// A certificate that does not verify will not verify in two seconds,
		// and retrying one gives a man-in-the-middle four chances.
		{"certificate verification", &tls.CertificateVerificationError{}, ClassTerminal},
		{"unknown authority", x509.UnknownAuthorityError{}, ClassTerminal},
		{"hostname mismatch", x509.HostnameError{}, ClassTerminal},
		{"invalid certificate", x509.CertificateInvalidError{}, ClassTerminal},

		// NXDOMAIN is terminal; a resolver that is still coming up is not.
		{"NXDOMAIN", &net.DNSError{Err: "no such host", IsNotFound: true}, ClassTerminal},
		{"SERVFAIL", &net.DNSError{Err: "server misbehaving", IsTemporary: true}, ClassTransient},
		{"resolver timeout", &net.DNSError{Err: "timeout", IsTimeout: true}, ClassTransient},

		{"a network timeout", &net.OpError{Op: "dial", Err: fakeTimeout{}}, ClassTransient},
		{"a truncated read", io.ErrUnexpectedEOF, ClassTransient},
		{"a 503", &StatusError{Code: 503}, ClassTransient},
		{"a 403", &StatusError{Code: 403}, ClassTerminal},

		// Unknown is TRANSIENT on purpose: an unnamed error is more likely a
		// network condition than a permanent refusal, a retry costs seconds,
		// and everything with a human-shaped next step is named above.
		{"something nobody has named", errors.New("the modem caught fire"), ClassTransient},
		{"nil", nil, ClassOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// recorder is a Retry with its jitter and sleep replaced, so the backoff
// schedule is a list rather than a wall-clock measurement.
func recorder(r Retry) (Retry, *[]time.Duration) {
	var slept []time.Duration
	r.jitter = func(d time.Duration) time.Duration { return d } // identity
	r.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return r, &slept
}

func TestDoReturnsATerminalFailureWithoutDelayOrWrapping(t *testing.T) {
	r, slept := recorder(DefaultRetry())
	calls := 0
	want := &StatusError{Code: http.StatusForbidden, Status: "403 Forbidden"}

	err := Do(context.Background(), r, func(context.Context, int) error {
		calls++
		return want
	})
	if calls != 1 {
		t.Errorf("a 403 was attempted %d times", calls)
	}
	if len(*slept) != 0 {
		t.Errorf("a terminal failure slept %v", *slept)
	}
	if err != want {
		// Unwrapped on purpose: the caller's next move is to show this to a
		// human, and a "gave up after 1 attempt" prefix on an author's opt-out
		// reads as a gvm failure rather than as the server's answer.
		t.Errorf("err = %v, want the StatusError itself", err)
	}
	if errors.Is(err, ErrExhausted) {
		t.Error("a terminal failure was reported as exhausted retries")
	}
}

func TestDoRetriesATransientFailureOnAnExponentialSchedule(t *testing.T) {
	r, slept := recorder(Retry{MaxAttempts: 5, Base: 100 * time.Millisecond, Max: 400 * time.Millisecond})
	calls := 0

	err := Do(context.Background(), r, func(_ context.Context, attempt int) error {
		calls++
		if attempt != calls {
			t.Errorf("attempt = %d on call %d", attempt, calls)
		}
		return &StatusError{Code: http.StatusServiceUnavailable, Status: "503"}
	})
	if calls != 5 {
		t.Errorf("attempts = %d, want 5", calls)
	}
	// Doubling, capped: 100, 200, 400, 400. Four sleeps for five attempts.
	want := []time.Duration{100, 200, 400, 400}
	if len(*slept) != len(want) {
		t.Fatalf("slept %v, want %d durations", *slept, len(want))
	}
	for i, w := range want {
		if (*slept)[i] != w*time.Millisecond {
			t.Errorf("sleep %d = %v, want %v", i, (*slept)[i], w*time.Millisecond)
		}
	}
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("err = %v, want ErrExhausted", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 503 {
		t.Errorf("the exhausted error lost the last failure: %v", err)
	}
}

func TestDoStopsAtTheFirstSuccess(t *testing.T) {
	r, slept := recorder(DefaultRetry())
	calls := 0
	err := Do(context.Background(), r, func(_ context.Context, attempt int) error {
		calls++
		if attempt < 2 {
			return &StatusError{Code: 502}
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("Do = %v after %d calls", err, calls)
	}
	if len(*slept) != 1 {
		t.Errorf("slept %v, want one backoff", *slept)
	}
}

func TestAServerRequestedRetryAfterOverridesAShorterBackoff(t *testing.T) {
	// Ignoring a Retry-After is how a keyless endpoint stops answering this IP
	// at all, so the server's number wins when it is the longer one -- and
	// loses when it is shorter, because the backoff is also protecting the
	// server from nineteen other goroutines.
	r, slept := recorder(Retry{MaxAttempts: 2, Base: 100 * time.Millisecond, Max: time.Second})
	_ = Do(context.Background(), r, func(context.Context, int) error {
		return &StatusError{Code: http.StatusTooManyRequests, RetryAfter: 3 * time.Second}
	})
	if len(*slept) != 1 || (*slept)[0] != 3*time.Second {
		t.Errorf("slept %v, want the server's 3s", *slept)
	}

	r2, slept2 := recorder(Retry{MaxAttempts: 2, Base: time.Second, Max: time.Second})
	_ = Do(context.Background(), r2, func(context.Context, int) error {
		return &StatusError{Code: http.StatusTooManyRequests, RetryAfter: 10 * time.Millisecond}
	})
	if len(*slept2) != 1 || (*slept2)[0] != time.Second {
		t.Errorf("slept %v, want the 1s backoff", *slept2)
	}
}

func TestTheOverallBudgetCutsTheLadderAndSaysSo(t *testing.T) {
	// MaxAttempts alone would let four attempts against a server that hangs
	// for its full timeout each time eat two minutes of downtime inside a
	// download cycle.
	r := Retry{MaxAttempts: 8, Base: time.Second, Max: 30 * time.Second, Total: 3 * time.Second}
	r.jitter = func(d time.Duration) time.Duration { return d }
	r.sleep = func(context.Context, time.Duration) error { return nil }

	calls := 0
	err := Do(context.Background(), r, func(context.Context, int) error {
		calls++
		return &StatusError{Code: 503}
	})
	if calls >= 8 {
		t.Errorf("the budget did not cut the ladder: %d attempts", calls)
	}
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
	if !errContains(err, "budget is spent") {
		t.Errorf("the refusal does not say the budget ran out: %v", err)
	}
}

func TestACancellationDuringBackoffKeepsTheCause(t *testing.T) {
	r := DefaultRetry()
	ctx, cancel := context.WithCancel(context.Background())
	r.jitter = func(d time.Duration) time.Duration { return d }
	r.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}

	calls := 0
	err := Do(ctx, r, func(context.Context, int) error {
		calls++
		return &StatusError{Code: 503, Status: "503 Service Unavailable"}
	})
	if calls != 1 {
		t.Errorf("attempts = %d after cancellation", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Error("the cancellation lost what was going wrong when the operator gave up")
	}
}

func TestDoRefusesALadderThatCannotTerminate(t *testing.T) {
	cases := []struct {
		name string
		r    Retry
	}{
		{"no attempts", Retry{MaxAttempts: 0, Base: time.Second, Max: time.Second}},
		{"a generous ladder is generous with downtime", Retry{MaxAttempts: 50, Base: time.Second, Max: time.Second}},
		{"a busy loop", Retry{MaxAttempts: 3, Base: 0, Max: time.Second}},
		{"a cap below the base", Retry{MaxAttempts: 3, Base: time.Second, Max: time.Millisecond}},
		{"a negative budget", Retry{MaxAttempts: 2, Base: time.Second, Max: time.Second, Total: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			err := Do(context.Background(), tc.r, func(context.Context, int) error {
				called = true
				return nil
			})
			if !errors.Is(err, ErrPolicy) {
				t.Fatalf("Do(%+v) = %v, want ErrPolicy", tc.r, err)
			}
			if called {
				t.Error("the function ran under an invalid ladder")
			}
		})
	}
	if err := DefaultRetry().Validate(); err != nil {
		t.Fatalf("the default ladder is invalid: %v", err)
	}
	// A single attempt is a meaningful policy and not an unset field.
	if err := (Retry{MaxAttempts: 1}).Validate(); err != nil {
		t.Errorf("MaxAttempts 1 with no backoff must be valid: %v", err)
	}
}

// TestFullJitterStaysInItsHalfOpenWindow. Without jitter, twenty instances off
// one nightly timer that all failed on the same CDN blip would retry in the
// same millisecond, forever -- which is the thundering herd that made the blip.
func TestFullJitterStaysInItsHalfOpenWindow(t *testing.T) {
	const d = time.Second
	var sawLow, sawHigh bool
	for range 200 {
		got := fullJitter(d)
		if got < d/2 || got > d {
			t.Fatalf("fullJitter(%v) = %v, outside [%v, %v]", d, got, d/2, d)
		}
		if got < d*3/4 {
			sawLow = true
		}
		if got > d*3/4 {
			sawHigh = true
		}
	}
	if !sawLow || !sawHigh {
		t.Error("fullJitter is not actually spreading; the herd is still synchronised")
	}
	if fullJitter(0) != 0 {
		t.Error("fullJitter(0) must be 0")
	}
}

func TestParseRetryAfterHandlesBothFormsRFC9110Allows(t *testing.T) {
	now := time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC)
	cases := []struct {
		name string
		in   string
		want time.Duration
		ok   bool
	}{
		{"delta seconds", "7", 7 * time.Second, true},
		{"fractional seconds", "2.5", 2500 * time.Millisecond, true},
		{"zero", "0", 0, true},
		{"negative", "-5", 0, true},
		// An HTTP-date, which CDNs send and which a seconds-only parser would
		// ignore -- looking exactly like a server that sent no header, so gvm
		// would retry straight into the next refusal.
		{"an HTTP date in the future", now.Add(20 * time.Second).UTC().Format(http.TimeFormat), 20 * time.Second, true},
		{"an HTTP date in the past", now.Add(-time.Hour).UTC().Format(http.TimeFormat), 0, true},
		// Capped: a Retry-After of a day inside a download cycle is not a
		// wait, it is a hang, and the operator needs to see the number.
		{"an absurd delay", "86400", maxRetryAfter, true},
		{"an absurd date", now.Add(24 * time.Hour).UTC().Format(http.TimeFormat), maxRetryAfter, true},
		{"nonsense", "soon", 0, false},
		{"empty", "  ", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRetryAfter(tc.in, now)
			if ok != tc.ok {
				t.Fatalf("ParseRetryAfter(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
			// A date is second-granular, so allow a second of slack there.
			if d := got - tc.want; d > time.Second || d < -time.Second {
				t.Errorf("ParseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestStatusErrorReadsLikeSomethingAnOperatorCanAct(t *testing.T) {
	se := &StatusError{
		Code:       http.StatusForbidden,
		Status:     "403 Forbidden",
		URL:        "https://api.curseforge.com/v1/mods/238222/files/5246076/download-url",
		Method:     http.MethodGet,
		RetryAfter: 0,
		Snippet:    `{"description":"not allowed"}`,
	}
	msg := se.Error()
	for _, want := range []string{"GET", "403 Forbidden", "api.curseforge.com", "not allowed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not carry %q: %s", want, msg)
		}
	}
	if !se.Blocked() {
		t.Error("a 403 on a download URL is section 12.8's distribution opt-out")
	}
	if (&StatusError{Code: 404}).Blocked() {
		t.Error("a 404 is a bad id, not an opt-out; three classifications, three messages")
	}
}

func errContains(err error, want string) bool {
	return err != nil && want != "" && strings.Contains(err.Error(), want)
}
