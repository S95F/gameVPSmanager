package runner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/probe"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Readiness is D-12: the replacement for TimeoutStartSec=infinity.
//
// Project Zomboid's first boot with a large workshop collection takes forty to
// sixty minutes, and the bash suite's answer was to wait forever. Waiting
// forever means a JVM that deadlocks with a silent console sits in
// `activating` until somebody notices, with Restart=on-failure never firing
// and no alert anywhere.
//
// Three independent bounds replace it, and all three are needed:
//
//  1. systemd's TimeoutStartSec, rendered from readiness.hard_deadline, is the
//     floor.
//  2. The runner extends it repeatedly with EXTEND_TIMEOUT_USEC, but ONLY
//     while progress is observable. An hour of workshop downloads keeps
//     extending; ten minutes of total silence does not.
//  3. hard_deadline is an absolute wall this runner enforces itself, because
//     an extension granted is an extension systemd will honour, and something
//     has to say no.
//
// The fourth bound is not a timer at all: a fatal fail_marker aborts the boot
// the moment the game prints it, with the manifest's hint. "Address already in
// use" is not worth waiting an hour to conclude.

const (
	// ExtendBy is how much extra time each extension asks for, and ExtendEvery
	// is how often. Every is a third of By so that two consecutive extensions
	// overlap: an extension that arrived exactly as the previous one expired
	// would be a race with systemd's own timer.
	ExtendBy    = 90 * time.Second
	ExtendEvery = 30 * time.Second
)

// ErrFatalMarker is what a readiness failure caused by a fail_marker wraps, so
// a caller can tell "the game told us it failed" from "we ran out of time".
var ErrFatalMarker = errors.New("runner: the game printed a fatal marker")

// ErrStalled is what a readiness failure caused by silence wraps.
var ErrStalled = errors.New("runner: the game stopped producing output")

// ErrStopRequested ends a readiness wait because somebody stopped the game,
// which is not a readiness failure. It is a distinct sentinel so the caller
// does not put a probe's name on an operator's decision -- nor exit with a
// code that makes Restart=on-failure start the world back up.
var ErrStopRequested = errors.New("runner: the game was stopped while it was still booting")

// Readiness drives the booting -> ready transition for one start.
//
// Every dependency is a value or an interface: the probes are built by the
// caller, the clock is injected, and progress is a function. So the whole of
// section 11.5 -- including the extension arithmetic that only ever runs under
// systemd -- is exercised on the development machine with a fake clock and no
// sleeping.
type Readiness struct {
	Spec spec.ReadinessSpec

	// Probes are run in order. Building them is the caller's job because
	// resolving a manifest port NAME to an address needs the instance's port
	// allocation, which this type has no business knowing.
	Probes []probe.Probe

	// Ring is where fail markers and progress lines are watched. nil disables
	// both, which is honest for a game with no console rather than a silent
	// downgrade to a blind timer.
	Ring *console.Ring

	Clock  console.Clock
	Notify *platform.Notifier
	Log    logger

	// Progress reports when the game last produced a byte. ANY byte counts:
	// section 11.5 is explicit that an hour of workshop download chatter is
	// progress even though none of it matches a marker.
	Progress func() time.Time

	// Extended accumulates the microseconds asked for, for status.json and the
	// shutdown report.
	Extended *atomic.Int64

	// Stop ends the wait when somebody stops the game mid-boot. nil never
	// fires, which is a wait with no way out but its own bounds.
	Stop <-chan struct{}

	// OnProbe is called with each probe's description as it starts, for
	// sd_notify STATUS= and status.json.
	OnProbe func(desc string)
}

func (rd *Readiness) clock() console.Clock {
	if rd.Clock == nil {
		return console.SystemClock{}
	}
	return rd.Clock
}

func (rd *Readiness) progressAt() time.Time {
	if rd.Progress == nil {
		return time.Time{}
	}
	return rd.Progress()
}

func (rd *Readiness) status(msg string) {
	if rd.Log != nil {
		rd.Log.Info("readiness: " + msg)
	}
	if rd.OnProbe != nil {
		rd.OnProbe(msg)
	}
}

// Await runs the probes and returns nil when the game is ready.
//
// The three bounds and the fail markers all race each other, and the FIRST
// answer wins. That ordering is deliberate: a fatal marker seen at 00:04 must
// not be held back until a probe's own timeout expires at 60:00.
func (rd *Readiness) Await(ctx context.Context) error {
	if rd.Spec.HardDeadline <= 0 {
		// spec's validator already refuses this, so reaching it means a
		// manifest got here another way. Refusing is the only safe answer: the
		// alternative is the infinity D-12 exists to remove.
		return errors.New("runner: readiness.hard_deadline is required and must be finite")
	}
	if len(rd.Probes) == 0 {
		return errors.New("runner: readiness has no probes, so nothing can ever send READY=1")
	}

	clk := rd.clock()
	start := clk.Now()
	deadline := start.Add(rd.Spec.HardDeadline.Duration())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Buffered so a watcher that loses the race still exits instead of
	// blocking forever on a channel nobody will read again.
	fail := make(chan error, 1)
	if rd.Ring != nil {
		markers, err := compileFailMarkers(rd.Spec.Fail)
		if err != nil {
			return err
		}
		if len(markers) > 0 {
			go rd.watchFail(ctx, markers, fail)
		}
	}

	bound := make(chan error, 1)
	go rd.extendLoop(ctx, start, deadline, bound)

	probes := make(chan error, 1)
	go func() {
		probes <- rd.runProbes(ctx)
	}()

	select {
	case <-rd.Stop:
		// Not a readiness failure. The game is already being stopped, and
		// recording a probe's name against an operator's decision would put
		// the wrong report on disk and the wrong exit code on the unit.
		return ErrStopRequested
	case err := <-fail:
		return err
	case err := <-bound:
		return err
	case err := <-probes:
		if err != nil {
			// A probe that ended because the deadline or a marker fired should
			// report THAT, not "context canceled". Give the racing watchers
			// the moment they need to deliver the real reason.
			select {
			case ferr := <-fail:
				return ferr
			case berr := <-bound:
				return berr
			default:
			}
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runProbes runs the probes in declaration order.
//
// Declaration order IS the dependency order, and `after:` is checked against
// it rather than used to build a graph. Every shipped manifest sequences its
// probes the way it lists them (Valheim's udp_probe is `after: log_marker` and
// is listed second), and a topological sort would let a manifest express an
// ordering its own reader cannot see -- while a forward reference is a
// manifest bug this catches instead of deadlocking on.
func (rd *Readiness) runProbes(ctx context.Context) error {
	seen := map[string]bool{}
	for i, p := range rd.Probes {
		if i < len(rd.Spec.Probes) {
			if after := strings.TrimSpace(rd.Spec.Probes[i].After); after != "" && !seen[after] {
				return fmt.Errorf("runner: readiness.probes[%d].after names %q, which is not an earlier probe", i, after)
			}
		}
		rd.status("waiting for " + p.Describe())

		var err error
		if p.Kind() == probe.KindLogMarker {
			// A log marker blocks on a ring subscription, so polling it would
			// only add latency to a boot an operator is watching.
			err = p.Run(ctx)
		} else {
			every := probe.DefaultEvery
			if i < len(rd.Spec.Probes) {
				every = rd.Spec.Probes[i].Every.Duration()
			}
			err = probe.Until(ctx, p, every, rd.clock())
		}
		if err != nil {
			return fmt.Errorf("readiness: %w", err)
		}
		seen[p.Kind()] = true
	}
	return nil
}

// extendLoop is the sketch in section 11.5, with the deadline and the stall
// clock as the two things that can end it.
//
// It sends the answer on out rather than returning it, because it races the
// probes: whichever finishes first is the truth about this boot.
func (rd *Readiness) extendLoop(ctx context.Context, start, deadline time.Time, out chan<- error) {
	if rd.Log != nil {
		defer func() {
			if rec := recover(); rec != nil {
				rd.Log.Error(fmt.Sprintf("readiness extend loop panicked: %v", rec))
			}
		}()
	}
	clk := rd.clock()
	stall := rd.Spec.StallTimeout.Duration()

	for {
		if err := sleepOn(ctx, clk, ExtendEvery); err != nil {
			return
		}
		now := clk.Now()
		if now.After(deadline) {
			out <- fmt.Errorf("readiness: hard_deadline %s exceeded without %s",
				rd.Spec.HardDeadline, rd.describe())
			return
		}
		if stall > 0 {
			if since := rd.silence(now, start); since > stall {
				// The §11.5 sketch returns nil here and lets systemd's
				// already-granted extension run out, up to ExtendBy later.
				// This reports the stall instead, and gives up that grace
				// deliberately: failing here is DETERMINISTIC -- the boot ends
				// at stall_timeout rather than at stall_timeout plus whatever
				// systemd happened to have left -- it runs the shutdown ladder
				// so the game is stopped cleanly and a Report is written, and
				// it keeps the hard_deadline check above alive, which
				// returning without an answer would silently retire along with
				// it. The cost is a server that goes quiet for stall_timeout
				// and would have spoken moments later; stall_timeout is the
				// manifest's statement of how long that is worth waiting.
				rd.status("stalled: no console output for " + since.Truncate(time.Second).String())
				out <- fmt.Errorf("readiness: %w for %s while waiting for %s",
					ErrStalled, since.Truncate(time.Second), rd.describe())
				return
			}
		}
		if rd.Notify != nil {
			if err := rd.Notify.ExtendTimeout(ExtendBy); err != nil && rd.Log != nil {
				rd.Log.Warn("sd_notify EXTEND_TIMEOUT_USEC failed: " + err.Error())
			}
		}
		if rd.Extended != nil {
			rd.Extended.Add(ExtendBy.Microseconds())
		}
	}
}

// silence is how long the game has been quiet.
//
// A zero last-byte time means the game has not printed anything at all yet, in
// which case the clock runs from the START OF THE WAIT rather than from the
// epoch -- otherwise the very first tick of a server that has not opened its
// mouth would read as fifty-six years of silence and fail the boot.
//
// Returning zero instead would disable the stall bound entirely until the
// first byte, which is the one case section 11.5 names by hand: "a JVM
// deadlocked with a silent console fails in ten minutes". Project Zomboid
// pairs stall_timeout 600s with hard_deadline 3600s, so that would be a six-
// fold overrun on exactly the boot the timeout was written for. Measuring from
// the wait's start rather than from spawn gives a pipe-mode game whose libc
// buffer has not flushed yet the full stall_timeout to produce something.
func (rd *Readiness) silence(now, start time.Time) time.Duration {
	last := rd.progressAt()
	if last.IsZero() {
		last = start
	}
	return now.Sub(last)
}

// describe names what is being waited for, for the status line and the
// deadline message. "hard_deadline 3600s exceeded" with no mention of what was
// awaited is the status line section 11.5 exists to replace.
func (rd *Readiness) describe() string {
	if len(rd.Probes) == 0 {
		return "any readiness probe"
	}
	return rd.Probes[len(rd.Probes)-1].Describe()
}

// failMarker is one compiled fail_markers entry.
type failMarker struct {
	re    *regexp.Regexp
	fatal bool
	hint  string
}

func compileFailMarkers(fs []spec.FailMarker) ([]failMarker, error) {
	out := make([]failMarker, 0, len(fs))
	for i, f := range fs {
		if f.Pattern == "" {
			continue
		}
		re, err := regexp.Compile(f.Pattern)
		if err != nil {
			return nil, fmt.Errorf("runner: readiness.fail_markers[%d].pattern: %w", i, err)
		}
		out = append(out, failMarker{re: re, fatal: f.Fatal, hint: f.Hint})
	}
	return out, nil
}

// watchFail aborts the boot the moment the game prints a fatal marker.
//
// It subscribes from Seq 1 rather than from now, because a marker printed
// while the probe list was being built is the same marker: "Address already in
// use" is usually the very first thing a server says.
//
// A non-fatal marker is annotated and the boot continues. That distinction is
// the manifest's to make and this code's to honour: `fatal: false` exists for
// the warnings that look alarming and are not.
func (rd *Readiness) watchFail(ctx context.Context, markers []failMarker, out chan<- error) {
	if rd.Log != nil {
		defer func() {
			if rec := recover(); rec != nil {
				rd.Log.Error(fmt.Sprintf("readiness fail-marker watcher panicked: %v", rec))
			}
		}()
	}
	sub := rd.Ring.Subscribe(1, 0)
	defer sub.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-sub.C:
			if !ok {
				return
			}
			lines := b.Lines
			if b.Dropped > 0 {
				// This watcher fell behind. Pay for a snapshot rather than
				// miss the one line that explains an hour of waiting.
				lines, _, _ = rd.Ring.Snapshot(0, 0)
			}
			for _, l := range lines {
				if l.Source != console.SourceGame {
					// Only the GAME's own output can fail its boot. A runner
					// annotation quoting a marker, or an operator typing one
					// into the console, must not abort a start.
					continue
				}
				for _, m := range markers {
					if !m.re.MatchString(l.Text) {
						continue
					}
					if !m.fatal {
						rd.status("warning marker: " + l.Text)
						continue
					}
					out <- markerError(l.Text, m.hint)
					return
				}
			}
		}
	}
}

// markerError renders a fatal marker with the manifest's hint attached.
//
// The hint is rendered verbatim and is the whole product difference between
// "java.lang.OutOfMemoryError" and "B42 native memory lives OUTSIDE the heap;
// raise memory_max or lower jvm_xmx" -- one of which an operator can act on at
// 2am.
func markerError(line, hint string) error {
	if hint == "" {
		return fmt.Errorf("%w: %s", ErrFatalMarker, line)
	}
	return fmt.Errorf("%w: %s: %s", ErrFatalMarker, line, hint)
}

// BuildProbes turns a manifest's readiness block into runnable probes.
//
// addr resolves a manifest port NAME to a dial address; it returns an error
// for a name the instance has no allocation for, because a probe silently
// skipped is a server reported ready before its socket is bound.
func BuildProbes(rs spec.ReadinessSpec, ring *console.Ring, addr func(port string) (string, error)) ([]probe.Probe, error) {
	out := make([]probe.Probe, 0, len(rs.Probes))
	for i, p := range rs.Probes {
		field := fmt.Sprintf("readiness.probes[%d]", i)
		switch p.Kind {
		case probe.KindLogMarker:
			if ring == nil {
				return nil, fmt.Errorf("runner: %s is a log_marker but this instance captures no console", field)
			}
			// since 0: the marker may already have scrolled past while the
			// probe list was being built, and a boot that re-waits for a line
			// the game printed two seconds ago waits the whole hour.
			m, err := probe.NewLogMarker(ring, p.Pattern, 0)
			if err != nil {
				return nil, fmt.Errorf("runner: %s: %w", field, err)
			}
			m.Timeout = p.Timeout.Duration()
			out = append(out, m)

		case probe.KindTCP, probe.KindUDP:
			if addr == nil {
				return nil, fmt.Errorf("runner: %s needs a port resolver", field)
			}
			a, err := addr(p.Port)
			if err != nil {
				return nil, fmt.Errorf("runner: %s: %w", field, err)
			}
			if p.Kind == probe.KindTCP {
				out = append(out, &probe.TCP{Port: p.Port, Addr: a, Timeout: p.Timeout.Duration()})
			} else {
				out = append(out, &probe.UDP{Port: p.Port, Addr: a, Timeout: p.Timeout.Duration()})
			}

		default:
			// a2s, mc_ping and control_auth are declared SupportImplemented in
			// spec's registry and are not built here yet. Saying so is the
			// point: skipping an unknown probe would send READY=1 for a check
			// that never ran, which is exactly the lie readiness exists to
			// prevent.
			return nil, fmt.Errorf("runner: %s kind %q is not built in this release", field, p.Kind)
		}
	}
	return out, nil
}

// AwaitReady runs the readiness block for this runner and moves it to `ready`.
//
// On success it sends READY=1, which is what ends systemd's TimeoutStartSec
// and what makes `systemctl start` return. On failure the runner is left in
// `booting` for the caller to decide about: the game is still running, and the
// decision to stop it belongs to the ladder, not to a probe.
func (r *Runner) AwaitReady(ctx context.Context, addr func(port string) (string, error)) error {
	rs := r.cfg.Manifest.Spec.Readiness
	probes, err := BuildProbes(rs, r.ring, addr)
	if err != nil {
		return err
	}
	rd := &Readiness{
		Spec:     rs,
		Probes:   probes,
		Ring:     r.ring,
		Clock:    r.clock,
		Notify:   r.notify,
		Log:      r.log,
		Progress: r.ingest.LastByte,
		Extended: &r.extendedUsec,
		OnProbe:  r.setProbe,
		Stop:     r.StopRequested(),
	}
	if err := rd.Await(ctx); err != nil {
		r.Note("-- gvm: not ready: %v", err)
		return err
	}

	r.SetState(StateReady)
	if r.notify != nil {
		if err := r.notify.Ready(); err != nil {
			r.log.Warn("sd_notify READY=1", "err", err)
		}
	}
	return nil
}
