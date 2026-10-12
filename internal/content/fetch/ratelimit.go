package fetch

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Limiter is a token bucket, written here rather than taken from
// golang.org/x/time/rate.
//
// Not because that package is wrong -- it is the obvious choice -- but because
// the dependency set is four modules and this is forty lines. The thing that
// would justify the dependency is correctness under concurrency, and that is
// one mutex.
//
// It is shared between the goroutines of one source's client, which is what
// makes it a rate limit rather than a per-request sleep: a collection sync that
// fanned out over twenty goroutines, each politely sleeping 250ms, would still
// hit the API twenty times at once.
type Limiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
	// floor is a pause the SERVER asked for: a 429's Retry-After, or an
	// exhausted X-Ratelimit-Remaining. It applies to every caller of this
	// limiter, not just the one that got the 429, because the limit being
	// enforced is per-IP and the other nineteen goroutines are the same IP.
	floor time.Time

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewLimiter returns a limiter refilling at rate tokens per second with a
// bucket depth of burst.
//
// A non-positive rate or burst is clamped rather than refused: this is
// constructed from a validated policy, and a limiter that returned an error
// would push its one caller into "if the limiter fails, skip it".
func NewLimiter(rate float64, burst int) *Limiter {
	if rate <= 0 {
		rate = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &Limiter{
		rate:  rate,
		burst: float64(burst),
		// Full at construction. An empty bucket would make the first request
		// of every process wait, and gvm's processes are short-lived jobs: the
		// burst would never be usable at all.
		tokens: float64(burst),
		now:    time.Now,
		sleep:  sleepCtx,
	}
}

// Wait blocks until a token is available or ctx is done.
//
// It returns ctx.Err() on cancellation and does NOT consume a token in that
// case, so a cancelled job does not spend budget that the next one would then
// wait for.
func (l *Limiter) Wait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Bounded, and the bound is not defensive padding: the loop's progress
	// depends on a clock and a sleep that a test supplies. A fake clock that
	// does not advance would spin a core forever, and the failure an author
	// wants in that case is a message, not a hang.
	for range 1024 {
		if err := ctx.Err(); err != nil {
			return err
		}
		wait, ok := l.take()
		if ok {
			return nil
		}
		if err := l.sleep(ctx, wait); err != nil {
			return err
		}
	}
	return fmt.Errorf("fetch: the rate limiter made no progress in 1024 rounds; its clock is "+
		"not advancing (rate %.2f/s, burst %.0f)", l.rate, l.burst)
}

// take grants a token, or reports how long until one is free.
func (l *Limiter) take() (wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if l.last.IsZero() {
		l.last = now
	}
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens += elapsed.Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
	}

	// A server-requested pause outranks the bucket. Honouring the bucket first
	// would send the request the server just told us not to send.
	if !l.floor.IsZero() && now.Before(l.floor) {
		return l.floor.Sub(now), false
	}
	if l.tokens >= 1 {
		l.tokens--
		return 0, true
	}
	// Round up: a wait computed to the nanosecond leaves the caller arriving
	// one tick early and looping, which turns one sleep into two.
	need := (1 - l.tokens) / l.rate
	return time.Duration(need*float64(time.Second)) + time.Millisecond, false
}

// PauseFor makes every caller of this limiter wait at least d.
//
// This is where a 429's Retry-After lands. Applying it to the limiter rather
// than to the one failing request is the difference between backing off and
// taking turns being refused.
func (l *Limiter) PauseFor(d time.Duration) {
	if l == nil || d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	until := l.now().Add(d)
	if until.After(l.floor) {
		l.floor = until
	}
}

// Paused reports how long the server-requested pause still has to run, for a
// progress line that would otherwise look like a hang.
func (l *Limiter) Paused() time.Duration {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.floor.IsZero() {
		return 0
	}
	if d := l.floor.Sub(l.now()); d > 0 {
		return d
	}
	return 0
}

// Tokens is the bucket's current depth, for a test and for a doctor line.
func (l *Limiter) Tokens() float64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tokens
}

// sleepCtx is a cancellable sleep. time.Sleep would hold a job's lock past a
// ctrl-C, and `gvm content sync` is a command an operator interrupts.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
