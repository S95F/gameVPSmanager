// Package probe answers one question about a game server -- "is it up yet?" --
// in the four ways the manifest's readiness block can ask it.
//
// Health checking is deliberately NOT RCON's job (section 11.9). RCON commands
// execute on the game's main server thread, so a health poll over RCON is a
// health poll that costs the players TPS, and on Project Zomboid the channel
// does not exist for the first forty minutes anyway -- which is precisely the
// window a readiness probe is for. So readiness is answered by the cheapest
// thing that can answer it: a log marker (universal, zero cost, works during
// boot), a TCP connect, or a UDP datagram.
//
// This package is pure Go and has no build tags. net.Dial works the same on the
// Windows development box as on the Debian target, and the one genuinely
// platform-shaped thing here -- what a UDP socket reports when nothing is
// listening -- is split into a pure classifier so the decision is tested
// everywhere and only the syscall differs.
package probe

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
)

// ErrNotReady is wrapped by every probe failure. The readiness driver retries
// on it until the hard deadline; nothing else in the system distinguishes "the
// port is closed" from "the port refused us", because at 00:04 into a boot they
// mean the same thing.
var ErrNotReady = errors.New("probe: not ready")

// Probe is one readiness check.
//
// Run returns nil when the probe is satisfied. Every probe applies its own
// timeout, so a caller sequencing probes does not have to remember which ones
// block: a manifest that says `timeout: 60s` gets 60 s whether the driver
// wrapped the context or not.
type Probe interface {
	// Kind is the manifest's `readiness.probes[].kind`.
	Kind() string

	// Describe is the sentence sd_notify STATUS= carries and the one that ends
	// up in "hard_deadline 3600s exceeded without <this>". It names what is
	// being waited for, not what the code is doing.
	Describe() string

	Run(ctx context.Context) error
}

// Failure is one probe's negative answer, carrying enough to write the status
// line without the caller reconstructing it.
type Failure struct {
	Kind   string
	Target string
	Detail string
	Err    error
}

func (f *Failure) Error() string {
	var b []byte
	b = append(b, f.Kind...)
	if f.Target != "" {
		b = append(b, ' ')
		b = append(b, f.Target...)
	}
	if f.Detail != "" {
		b = append(b, ':', ' ')
		b = append(b, f.Detail...)
	}
	if f.Err != nil {
		b = append(b, ':', ' ')
		b = append(b, f.Err.Error()...)
	}
	return string(b)
}

// Unwrap returns both, so errors.Is(err, ErrNotReady) is true for every probe
// failure AND errors.Is(err, context.DeadlineExceeded) still works on the
// underlying cause. A caller that had to choose would lose one of them.
func (f *Failure) Unwrap() []error {
	if f.Err != nil {
		return []error{ErrNotReady, f.Err}
	}
	return []error{ErrNotReady}
}

func fail(kind, target, detail string, err error) error {
	return &Failure{Kind: kind, Target: target, Detail: detail, Err: err}
}

// DefaultEvery is the retry interval when a manifest does not name one. Two
// seconds: fast enough that a server which came up during the gap is noticed
// within one status.json rewrite, slow enough not to be a connect storm against
// a game that is still loading a world.
const DefaultEvery = 2 * time.Second

// Until retries p every `every` until it succeeds or ctx ends.
//
// The clock is injected because the alternative is a test suite whose runtime
// is measured in the intervals it is testing. c may be nil for the real clock.
func Until(ctx context.Context, p Probe, every time.Duration, c console.Clock) error {
	if every <= 0 {
		every = DefaultEvery
	}
	if c == nil {
		c = console.SystemClock{}
	}
	var last error
	for {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return fmt.Errorf("%s: %w", p.Describe(), last)
			}
			return err
		}
		err := p.Run(ctx)
		if err == nil {
			return nil
		}
		last = err
		if err := sleep(ctx, c, every); err != nil {
			return fmt.Errorf("%s: %w", p.Describe(), last)
		}
	}
}

// sleep waits d or until ctx ends, on the injected clock.
func sleep(ctx context.Context, c console.Clock, d time.Duration) error {
	done := make(chan struct{})
	t := c.AfterFunc(d, func() { close(done) })
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		t.Stop()
		return ctx.Err()
	}
}

// withTimeout applies a probe's own timeout on top of the caller's context,
// taking whichever is tighter -- which is what context.WithTimeout already
// does, so this exists only to make "0 means the caller's deadline" one
// decision instead of three copies of it.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
