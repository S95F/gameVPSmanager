package fetch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is a clock that only moves when something sleeps, which is what
// makes a rate limiter's spacing assertable instead of approximately asserted.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	slept  []time.Duration
	freeze bool // sleep records the duration and does NOT advance
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	if !c.freeze {
		c.now = c.now.Add(d)
	}
	return nil
}

func (c *fakeClock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

func (c *fakeClock) total() time.Duration {
	var sum time.Duration
	for _, d := range c.Slept() {
		sum += d
	}
	return sum
}

func onClock(l *Limiter, c *fakeClock) *Limiter {
	l.now = c.Now
	l.sleep = c.Sleep
	return l
}

// TestTheBucketStartsFullSoAShortJobCanUseItsBurst. gvm's fetches happen in
// short-lived job processes: a limiter that started empty would make the first
// request of every job wait, and the burst would never be usable at all.
func TestTheBucketStartsFullSoAShortJobCanUseItsBurst(t *testing.T) {
	c := newFakeClock()
	l := onClock(NewLimiter(2, 5), c)

	for i := range 5 {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if got := c.total(); got != 0 {
		t.Fatalf("the first 5 requests against a burst of 5 slept %v", got)
	}
	// The sixth has to wait for a refill: 2 per second is 500ms a token.
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.total(); got < 500*time.Millisecond {
		t.Errorf("the sixth request slept %v, want at least 500ms", got)
	}
}

func TestTheRateIsEnforcedAcrossGoroutinesAndNotPerRequest(t *testing.T) {
	// The reason this is a shared limiter rather than a per-request sleep: a
	// collection sync that fanned out over twenty goroutines, each politely
	// sleeping, would still hit the API twenty times at once.
	c := newFakeClock()
	l := onClock(NewLimiter(10, 1), c)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = l.Wait(context.Background())
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	// One token was in the bucket; the other seven each cost 100ms.
	if got := c.total(); got < 600*time.Millisecond {
		t.Errorf("8 requests at 10/s with burst 1 slept %v in total, want at least 600ms", got)
	}
}

func TestAServerRequestedPauseOutranksTheBucket(t *testing.T) {
	c := newFakeClock()
	l := onClock(NewLimiter(1000, 100), c)

	// The bucket is full, so without the pause this would not sleep at all.
	l.PauseFor(7 * time.Second)
	if got := l.Paused(); got != 7*time.Second {
		t.Fatalf("Paused = %v, want 7s", got)
	}
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.total(); got < 7*time.Second {
		t.Errorf("slept %v through a 7s server-requested pause", got)
	}
	if got := l.Paused(); got != 0 {
		t.Errorf("Paused = %v after the pause expired", got)
	}

	// A longer pause extends; a shorter one does not shorten. A 429 from one
	// goroutine must not be undone by a cheerful header from another.
	l.PauseFor(5 * time.Second)
	l.PauseFor(2 * time.Second)
	if got := l.Paused(); got != 5*time.Second {
		t.Errorf("Paused = %v, want the longer of the two pauses", got)
	}
	l.PauseFor(0)
	if got := l.Paused(); got != 5*time.Second {
		t.Errorf("PauseFor(0) changed the floor to %v", got)
	}
}

func TestWaitHonoursCancellationAndSpendsNoToken(t *testing.T) {
	c := newFakeClock()
	l := onClock(NewLimiter(1, 1), c)

	// Drain the bucket so the next Wait has to sleep.
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := l.Tokens()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait(cancelled) = %v, want context.Canceled", err)
	}
	if after := l.Tokens(); after != before {
		t.Errorf("a cancelled Wait spent budget: %v -> %v; the next job would then wait for "+
			"a request that never happened", before, after)
	}
}

// TestAFrozenClockIsReportedRatherThanSpun. The loop's progress depends on a
// clock and a sleep a caller supplies. A fake clock that does not advance would
// otherwise peg a core forever, and the failure an author wants in that case is
// a message.
func TestAFrozenClockIsReportedRatherThanSpun(t *testing.T) {
	c := newFakeClock()
	c.freeze = true
	l := onClock(NewLimiter(1, 1), c)

	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := l.Wait(context.Background())
	if err == nil {
		t.Fatal("a frozen clock was not reported")
	}
	if got := len(c.Slept()); got > 1100 {
		t.Errorf("the limiter slept %d times before giving up", got)
	}
}

func TestNewLimiterClampsRatherThanRefuses(t *testing.T) {
	// Built from an already-validated policy, so an error return here would
	// only push its one caller into "if the limiter fails, skip it".
	for _, l := range []*Limiter{NewLimiter(0, 0), NewLimiter(-1, -5)} {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("a clamped limiter refused the first request: %v", err)
		}
	}
}

func TestTheRealSleepActuallyWaitsAndIsCancellable(t *testing.T) {
	// The fake clock everywhere else means nothing here is ever exercised, so
	// the one place the production sleep runs is asserted directly.
	l := NewLimiter(50, 1)
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Errorf("the second request at 50/s returned after %v", elapsed)
	}

	l2 := NewLimiter(1, 1)
	if err := l2.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l2.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait with an expiring context = %v; time.Sleep here would hold a job's "+
			"lock past a ctrl-C", err)
	}
}

func TestNilLimiterIsANoOp(t *testing.T) {
	var l *Limiter
	if err := l.Wait(context.Background()); err != nil {
		t.Errorf("nil Wait = %v", err)
	}
	l.PauseFor(time.Second)
	if l.Paused() != 0 || l.Tokens() != 0 {
		t.Error("a nil limiter claimed state")
	}
}
