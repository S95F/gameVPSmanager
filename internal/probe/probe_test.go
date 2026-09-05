package probe

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
)

// countingProbe fails its first failUntil runs, then succeeds.
type countingProbe struct {
	failUntil int32
	runs      atomic.Int32
	err       error
}

func (p *countingProbe) Kind() string     { return "counting" }
func (p *countingProbe) Describe() string { return "a probe that has been asked " + "n times" }

func (p *countingProbe) Run(context.Context) error {
	if p.runs.Add(1) <= p.failUntil {
		if p.err != nil {
			return p.err
		}
		return fail("counting", "", "not yet", nil)
	}
	return nil
}

func TestUntilRetriesUntilSuccess(t *testing.T) {
	p := &countingProbe{failUntil: 3}
	if err := Until(context.Background(), p, time.Millisecond, nil); err != nil {
		t.Fatalf("Until: %v", err)
	}
	if got := p.runs.Load(); got != 4 {
		t.Fatalf("ran %d times, want 4", got)
	}
}

// The clock is injected so a probe whose manifest says `every: 60s` is tested
// in microseconds rather than in minutes. This test would take four hours on a
// real clock.
func TestUntilUsesTheInjectedClock(t *testing.T) {
	fc := console.NewFakeClock(time.Unix(1_700_000_000, 0).UTC())
	p := &countingProbe{failUntil: 4}

	done := make(chan error, 1)
	go func() { done <- Until(context.Background(), p, time.Hour, fc) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Until: %v", err)
			}
			if got := p.runs.Load(); got != 5 {
				t.Fatalf("ran %d times, want 5", got)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Until never finished; it is not waiting on the injected clock")
		}
		fc.Advance(time.Hour)
	}
}

// When the deadline wins, the error must be the PROBE's last complaint, not a
// bare context.DeadlineExceeded. "hard_deadline 3600s exceeded" with no mention
// of what was being waited for is the status line section 11.5 exists to
// replace.
func TestUntilReportsTheLastProbeFailure(t *testing.T) {
	boom := errors.New("connect: connection refused")
	p := &countingProbe{failUntil: 1 << 30, err: fail("tcp_probe", "127.0.0.1:27015", "connect failed", boom)}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := Until(ctx, p, time.Millisecond, nil)
	if err == nil {
		t.Fatal("Until returned success on a probe that never passed")
	}
	if !errors.Is(err, boom) {
		t.Errorf("the underlying cause is lost: %v", err)
	}
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("error does not wrap ErrNotReady: %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:27015") {
		t.Errorf("error does not name what was being waited for: %v", err)
	}
}

// A context that is already dead must not run the probe at all: a readiness
// loop that has hit its hard deadline should not open one more connection.
func TestUntilRespectsAnAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := &countingProbe{}
	if err := Until(ctx, p, time.Millisecond, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Until: %v, want context.Canceled", err)
	}
	if got := p.runs.Load(); got != 0 {
		t.Fatalf("ran %d times on a cancelled context", got)
	}
}

func TestFailureFormatsUsefully(t *testing.T) {
	cause := errors.New("connect: connection refused")
	err := fail(KindTCP, "127.0.0.1:27015", "connect failed", cause)

	want := "tcp_probe 127.0.0.1:27015: connect failed: connect: connection refused"
	if err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, ErrNotReady) || !errors.Is(err, cause) {
		t.Fatal("a Failure must satisfy errors.Is for both ErrNotReady and its cause")
	}

	bare := fail(KindLogMarker, "", "not seen", nil)
	if bare.Error() != "log_marker: not seen" {
		t.Fatalf("Error() = %q", bare.Error())
	}
	if !errors.Is(bare, ErrNotReady) {
		t.Fatal("a causeless Failure is not ErrNotReady")
	}
}
