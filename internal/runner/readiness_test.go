package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/probe"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Readiness is D-12, and the whole point of these tests is that not one of
// them sleeps. The extension arithmetic only ever runs under systemd on a boot
// that takes an hour; asserting it with real time would mean asserting it
// never.

type readyKit struct {
	*Readiness
	fake     *console.Fake
	clock    *console.FakeClock
	extended atomic.Int64
	progress atomic.Int64 // unix nanos; 0 means "the game has said nothing"
}

func newReadyKit(t *testing.T, rs spec.ReadinessSpec, pattern string) *readyKit {
	t.Helper()
	clock := console.NewFakeClock(time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC))
	f := console.NewFake(console.FakeOptions{Clock: clock, PartialAfter: -1})
	t.Cleanup(func() { _ = f.Close() })

	k := &readyKit{fake: f, clock: clock}
	m, err := probe.NewLogMarker(f.Ring, pattern, 0)
	if err != nil {
		t.Fatal(err)
	}
	k.Readiness = &Readiness{
		Spec:     rs,
		Probes:   []probe.Probe{m},
		Ring:     f.Ring,
		Clock:    clock,
		Log:      slog.New(slog.DiscardHandler),
		Extended: &k.extended,
		Progress: k.lastByte,
	}
	return k
}

func (k *readyKit) lastByte() time.Time {
	n := k.progress.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// observe records that the game produced a byte now, which is what the ingest
// does on ANY output: section 11.5 is explicit that an hour of workshop
// download chatter is progress even though none of it matches a marker.
func (k *readyKit) observe() { k.progress.Store(k.clock.Now().UnixNano()) }

func (k *readyKit) await(t *testing.T) <-chan error {
	t.Helper()
	out := make(chan error, 1)
	go func() { out <- k.Await(context.Background()) }()
	return out
}

// tick expires the extend loop's next wakeup.
func (k *readyKit) tick(t *testing.T) {
	t.Helper()
	settle(t, "the extend loop to arm its next wakeup", func() bool { return k.clock.Timers() >= 1 })
	k.clock.Advance(ExtendEvery)
}

// tickUntil expires wakeups until Await answers, running before at each one.
//
// It tolerates a moment with no armed timer, because that is exactly the state
// the loop is in once it has decided to give up: the answer is already on its
// way and insisting on another wakeup would deadlock the test on the case it
// is testing.
func (k *readyKit) tickUntil(t *testing.T, ch <-chan error, before func()) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-ch:
			return err
		default:
		}
		if k.clock.Timers() >= 1 {
			if before != nil {
				before()
			}
			k.clock.Advance(ExtendEvery)
			continue
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatal("readiness never answered")
	return nil
}

func awaitErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("readiness never finished")
		return nil
	}
}

// TestReadinessExtendsWhileTheGameIsTalking. Project Zomboid's first boot with
// a large workshop collection runs forty to sixty minutes and prints the whole
// time; every one of those minutes has to be bought from systemd, or
// TimeoutStartSec kills the boot that was working.
func TestReadinessExtendsWhileTheGameIsTalking(t *testing.T) {
	k := newReadyKit(t, spec.ReadinessSpec{
		HardDeadline: spec.Duration(3600 * time.Second),
		StallTimeout: spec.Duration(600 * time.Second),
	}, "SERVER STARTED")
	k.observe()
	ch := k.await(t)

	const ticks = 5
	for range ticks {
		k.observe() // downloads still chattering
		k.tick(t)
	}
	settle(t, "the extensions to be counted", func() bool {
		return k.extended.Load() >= ticks*ExtendBy.Microseconds()
	})

	k.fake.Emit("*** SERVER STARTED ***")
	if err := awaitErr(t, ch); err != nil {
		t.Fatalf("Await = %v, want nil once the marker appeared", err)
	}
	if got, want := k.extended.Load(), int64(ticks)*ExtendBy.Microseconds(); got != want {
		t.Errorf("extended %d usec, want %d", got, want)
	}
}

// TestReadinessStopsExtendingOnSilence. A JVM deadlocked with a silent console
// must fail in ten minutes, not in an hour: the extension is bought with
// observed progress, and there is none.
func TestReadinessStopsExtendingOnSilence(t *testing.T) {
	k := newReadyKit(t, spec.ReadinessSpec{
		HardDeadline: spec.Duration(3600 * time.Second),
		StallTimeout: spec.Duration(60 * time.Second),
	}, "SERVER STARTED")
	k.observe() // one byte at the start, then nothing ever again
	ch := k.await(t)

	err := k.tickUntil(t, ch, nil)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("err = %v, want one wrapping ErrStalled", err)
	}
	if !strings.Contains(err.Error(), "SERVER STARTED") {
		t.Errorf("err = %q, want it to name what was being waited for", err)
	}
	// Three ticks of 30 s reach the 60 s stall clock, so at most three
	// extensions can have been bought before it fired.
	if got, max := k.extended.Load(), int64(3)*ExtendBy.Microseconds(); got > max {
		t.Errorf("extended %d usec after a stall; at most %d should have been asked for", got, max)
	}
}

// TestReadinessEnforcesTheHardDeadline. The runner enforces the wall itself:
// an extension granted is an extension systemd will honour, so something has
// to say no. This is the whole of D-12.
func TestReadinessEnforcesTheHardDeadline(t *testing.T) {
	k := newReadyKit(t, spec.ReadinessSpec{
		HardDeadline: spec.Duration(60 * time.Second),
	}, "SERVER STARTED")
	ch := k.await(t)

	// The game is still talking the whole time, and it makes no difference.
	err := k.tickUntil(t, ch, k.observe)
	if !strings.Contains(err.Error(), "hard_deadline") {
		t.Errorf("err = %q, want it to name the deadline", err)
	}
	if !strings.Contains(err.Error(), "SERVER STARTED") {
		t.Errorf("err = %q; 'hard_deadline exceeded' with no mention of what was awaited "+
			"is the status line section 11.5 exists to replace", err)
	}
}

// TestReadinessAbortsOnAFatalMarker. "Address already in use" is not worth
// waiting an hour to conclude, and the manifest's hint is what an operator can
// act on at 2am.
func TestReadinessAbortsOnAFatalMarker(t *testing.T) {
	const hint = "another server holds this port; check `gvm doctor` and `ss -lunp`"
	k := newReadyKit(t, spec.ReadinessSpec{
		HardDeadline: spec.Duration(3600 * time.Second),
		Fail: []spec.FailMarker{
			{Pattern: "Address already in use", Fatal: true, Hint: hint},
		},
	}, "SERVER STARTED")
	ch := k.await(t)

	k.fake.Emit("java.net.BindException: Address already in use")
	err := awaitErr(t, ch)
	if !errors.Is(err, ErrFatalMarker) {
		t.Fatalf("err = %v, want one wrapping ErrFatalMarker", err)
	}
	if !strings.Contains(err.Error(), hint) {
		t.Errorf("err = %q, want the manifest's hint verbatim", err)
	}
	if !strings.Contains(err.Error(), "BindException") {
		t.Errorf("err = %q, want the line the game actually printed", err)
	}
}

// TestReadinessIgnoresANonFatalMarker. `fatal: false` exists for the warnings
// that look alarming and are not, and the distinction is the manifest's to
// make.
func TestReadinessIgnoresANonFatalMarker(t *testing.T) {
	k := newReadyKit(t, spec.ReadinessSpec{
		HardDeadline: spec.Duration(3600 * time.Second),
		Fail:         []spec.FailMarker{{Pattern: "deprecated", Fatal: false, Hint: "harmless"}},
	}, "SERVER STARTED")
	ch := k.await(t)

	k.fake.Emit("WARN: option XYZ is deprecated")
	k.fake.Emit("*** SERVER STARTED ***")
	if err := awaitErr(t, ch); err != nil {
		t.Fatalf("Await = %v, want nil: a non-fatal marker must not fail a boot", err)
	}
}

// TestFailMarkersOnlyMatchTheGame. A runner annotation quoting a marker, or an
// operator typing one into the console, must not abort a start. Only the
// game's own output can fail its boot.
func TestFailMarkersOnlyMatchTheGame(t *testing.T) {
	k := newReadyKit(t, spec.ReadinessSpec{
		HardDeadline: spec.Duration(3600 * time.Second),
		Fail:         []spec.FailMarker{{Pattern: "Address already in use", Fatal: true}},
	}, "SERVER STARTED")
	ch := k.await(t)

	k.fake.Note("-- gvm: retrying after Address already in use")
	if _, err := k.fake.Send("user:alice", "grep 'Address already in use' log"); err != nil {
		t.Fatal(err)
	}
	k.fake.Emit("*** SERVER STARTED ***")
	if err := awaitErr(t, ch); err != nil {
		t.Fatalf("Await = %v, want nil: only game output may fail a boot", err)
	}
}

// TestReadinessMarkerAlreadyInTheRing. Unlike a shutdown rung, a readiness
// marker starts from Seq 0: it may already have scrolled past while the probe
// list was being built, and a boot that re-waits for a line printed two
// seconds ago waits the whole hour.
func TestReadinessMarkerAlreadyInTheRing(t *testing.T) {
	k := newReadyKit(t, spec.ReadinessSpec{
		HardDeadline: spec.Duration(3600 * time.Second),
	}, "SERVER STARTED")
	k.fake.Emit("*** SERVER STARTED ***")

	if err := awaitErr(t, k.await(t)); err != nil {
		t.Fatalf("Await = %v, want nil for a marker already in the ring", err)
	}
}

func TestReadinessRefusesAnInfiniteDeadline(t *testing.T) {
	k := newReadyKit(t, spec.ReadinessSpec{}, "SERVER STARTED")
	err := k.Await(context.Background())
	if err == nil || !strings.Contains(err.Error(), "hard_deadline") {
		t.Fatalf("err = %v, want a refusal: an absent deadline is the infinity D-12 removes", err)
	}
}

func TestReadinessRefusesAForwardAfterReference(t *testing.T) {
	k := newReadyKit(t, spec.ReadinessSpec{
		HardDeadline: spec.Duration(3600 * time.Second),
		Probes:       []spec.ReadyProbe{{Kind: probe.KindLogMarker, Pattern: "x", After: "udp_probe"}},
	}, "SERVER STARTED")
	err := awaitErr(t, k.await(t))
	if err == nil || !strings.Contains(err.Error(), "not an earlier probe") {
		t.Fatalf("err = %v, want a refusal naming the forward reference", err)
	}
}

func TestBuildProbes(t *testing.T) {
	f := console.NewFake(console.FakeOptions{PartialAfter: -1})
	t.Cleanup(func() { _ = f.Close() })
	addr := func(port string) (string, error) {
		if port != "game" {
			return "", fmt.Errorf("there is no port named %q", port)
		}
		return "127.0.0.1:16261", nil
	}

	t.Run("builds the shipped shape", func(t *testing.T) {
		got, err := BuildProbes(spec.ReadinessSpec{Probes: []spec.ReadyProbe{
			{Kind: probe.KindLogMarker, Pattern: "SERVER STARTED", Timeout: spec.Duration(3600 * time.Second)},
			{Kind: probe.KindUDP, Port: "game", After: "log_marker", Timeout: spec.Duration(60 * time.Second)},
		}}, f.Ring, addr)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Kind() != probe.KindLogMarker || got[1].Kind() != probe.KindUDP {
			t.Fatalf("kinds = %v", got)
		}
		if !strings.Contains(got[1].Describe(), "16261") {
			t.Errorf("describe = %q, want the resolved address", got[1].Describe())
		}
	})

	for _, tc := range []struct {
		name  string
		probe spec.ReadyProbe
		ring  *console.Ring
		want  string
	}{
		{"unbuilt kind", spec.ReadyProbe{Kind: "control_auth", Port: "game"}, f.Ring, "not built in this release"},
		{"unknown port", spec.ReadyProbe{Kind: probe.KindTCP, Port: "rcon"}, f.Ring, "no port named"},
		{"marker with no console", spec.ReadyProbe{Kind: probe.KindLogMarker, Pattern: "x"}, nil, "captures no console"},
		{"bad pattern", spec.ReadyProbe{Kind: probe.KindLogMarker, Pattern: "("}, f.Ring, "error parsing regexp"},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			_, err := BuildProbes(spec.ReadinessSpec{Probes: []spec.ReadyProbe{tc.probe}}, tc.ring, addr)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// TestEveryImplementedProbeKindIsBuilt keeps spec's probe registry and the
// runner's BuildProbes in step: a kind spec declares SupportImplemented must be
// constructible here, or a manifest validate accepts would send READY=1 for a
// readiness check that never ran.
func TestEveryImplementedProbeKindIsBuilt(t *testing.T) {
	f := console.NewFake(console.FakeOptions{PartialAfter: -1})
	t.Cleanup(func() { _ = f.Close() })
	addr := func(string) (string, error) { return "127.0.0.1:16261", nil }

	kinds := spec.ImplementedProbeKinds()
	if len(kinds) < 3 {
		t.Fatalf("only %d implemented probe kinds; the registry read is wrong", len(kinds))
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			got, err := BuildProbes(spec.ReadinessSpec{Probes: []spec.ReadyProbe{
				{Kind: kind, Port: "game", Pattern: "ready", Timeout: spec.Duration(time.Second)},
			}}, f.Ring, addr)
			if err != nil {
				t.Fatalf("spec declares %q implemented, but BuildProbes cannot build it: %v", kind, err)
			}
			if len(got) != 1 || got[0].Kind() != kind {
				t.Fatalf("built %v for kind %q", got, kind)
			}
		})
	}
}

func TestReadinessSilenceIgnoresAGameThatHasSaidNothing(t *testing.T) {
	k := newReadyKit(t, spec.ReadinessSpec{
		HardDeadline: spec.Duration(3600 * time.Second),
		StallTimeout: spec.Duration(60 * time.Second),
	}, "x")
	// progress is zero: the game has not printed a byte yet. Reading that as
	// the epoch would score fifty-six years of silence on the first tick and
	// fail every boot before its first line of output. The clock therefore
	// runs from the START OF THE WAIT -- which is not the same as not running
	// at all, because a game that never opens its mouth is exactly the case
	// stall_timeout exists for.
	start := k.clock.Now()
	if got := k.silence(start, start); got != 0 {
		t.Errorf("silence = %s at the start of the wait, want 0", got)
	}
	if got, want := k.silence(start.Add(30*time.Second), start), 30*time.Second; got != want {
		t.Errorf("silence = %s, want %s -- not the epoch, and not zero forever", got, want)
	}
}

// TestReadinessStallsAGameThatNeverPrintsAByte.
//
// Project Zomboid pairs hard_deadline 3600s with stall_timeout 600s. A JVM
// that deadlocks before its first write -- the case section 11.5 names, "a JVM
// deadlocked with a silent console fails in ten minutes" -- must fail at the
// stall timeout, not extend systemd's timeout every 30 s for the full hour.
func TestReadinessStallsAGameThatNeverPrintsAByte(t *testing.T) {
	k := newReadyKit(t, spec.ReadinessSpec{
		HardDeadline: spec.Duration(3600 * time.Second),
		StallTimeout: spec.Duration(600 * time.Second),
	}, "SERVER STARTED")
	// No observe(): nothing is ever written, which is the whole point.
	start := k.clock.Now()
	ch := k.await(t)

	err := k.tickUntil(t, ch, nil)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("err = %v, want one wrapping ErrStalled", err)
	}
	if elapsed := k.clock.Now().Sub(start); elapsed > 700*time.Second {
		t.Errorf("stalled only after %s; stall_timeout is 600s and hard_deadline is an hour away", elapsed)
	}
}

// TestAwaitReadyEndsInREADY wires the whole thing to a runner: the game prints
// its marker on its real stdout, the pump ingests it, and the unit becomes
// ready. This is the path `systemctl start` blocks on.
func TestAwaitReadyEndsInREADY(t *testing.T) {
	h := newHarness(t)
	h.cfg.Manifest.Spec.Readiness = spec.ReadinessSpec{
		HardDeadline: spec.Duration(3600 * time.Second),
		StallTimeout: spec.Duration(600 * time.Second),
		Probes:       []spec.ReadyProbe{{Kind: probe.KindLogMarker, Pattern: "SERVER STARTED"}},
	}
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.AwaitReady(context.Background(), nil) }()

	settle(t, "the probe to be published", func() bool {
		return strings.Contains(h.Snapshot().Readiness.Probe, "SERVER STARTED")
	})
	if _, err := io.WriteString(h.out, "*** SERVER STARTED ***\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AwaitReady = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AwaitReady never returned")
	}
	if got := h.State(); got != StateReady {
		t.Errorf("state = %q, want %q", got, StateReady)
	}
	if h.ReadyAt().IsZero() {
		t.Error("ready_at must be stamped when the unit becomes ready")
	}
}

// TestAwaitReadyReportsAFatalMarker. The runner stays in `booting` on failure:
// the game is still running, and the decision to stop it belongs to the
// ladder, not to a probe.
func TestAwaitReadyReportsAFatalMarker(t *testing.T) {
	h := newHarness(t)
	h.cfg.Manifest.Spec.Readiness = spec.ReadinessSpec{
		HardDeadline: spec.Duration(3600 * time.Second),
		Probes:       []spec.ReadyProbe{{Kind: probe.KindLogMarker, Pattern: "SERVER STARTED"}},
		Fail:         []spec.FailMarker{{Pattern: "Address already in use", Fatal: true, Hint: "check `ss -lunp`"}},
	}
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.AwaitReady(context.Background(), nil) }()

	settle(t, "the probe to start", func() bool {
		return h.Snapshot().Readiness.Probe != ""
	})
	if _, err := io.WriteString(h.out, "BindException: Address already in use\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrFatalMarker) || !strings.Contains(err.Error(), "ss -lunp") {
			t.Fatalf("err = %v, want a fatal marker carrying the hint", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AwaitReady never returned")
	}
	if got := h.State(); got != StateBooting {
		t.Errorf("state = %q, want %q: stopping the game is the ladder's decision", got, StateBooting)
	}
}

// TestAStopDuringTheBootEndsTheReadinessWait.
//
// A socket `stop` runs the ladder on the connection's goroutine and never
// touches readiness, so `gvm run` stayed blocked in AwaitReady on a game the
// operator had already stopped. Project Zomboid's first boot is forty minutes
// long, and the stall clock only advances on GAME bytes -- the ladder's own
// annotations do not refresh it -- so the runner idled to stall_timeout, then
// ran a SECOND ladder over the corpse, overwrote the operator's report with a
// bogus "readiness failed" one, and exited 75, which is absent from
// RestartPreventExitStatus and therefore restarts the world somebody stopped.
func TestAStopDuringTheBootEndsTheReadinessWait(t *testing.T) {
	k := newReadyKit(t, spec.ReadinessSpec{
		HardDeadline: spec.Duration(3600 * time.Second),
		StallTimeout: spec.Duration(600 * time.Second),
	}, "SERVER STARTED")
	stop := make(chan struct{})
	k.Readiness.Stop = stop
	k.observe()
	ch := k.await(t)

	close(stop)
	err := awaitErr(t, ch)
	if !errors.Is(err, ErrStopRequested) {
		t.Fatalf("err = %v, want one wrapping ErrStopRequested", err)
	}
	// Nothing may have been bought from systemd: the boot ended immediately,
	// not at the stall timeout ten minutes later.
	if got := k.extended.Load(); got != 0 {
		t.Errorf("extended %d usec after a stop; the wait should have ended at once", got)
	}
}

// The runner-level wiring: Shutdown is what closes the channel, and a runner
// that has never been asked to stop must not see one.
func TestStopRequestedIsClosedByTheLadderAndNotBefore(t *testing.T) {
	h := newHarness(t)
	select {
	case <-h.StopRequested():
		t.Fatal("StopRequested is closed on a runner nobody has stopped")
	default:
	}
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	h.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }
	h.Shutdown(context.Background(), "user:alice", "operator stop")
	select {
	case <-h.StopRequested():
	default:
		t.Error("StopRequested is still open after a ladder ran")
	}
}
