package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Every test in this file runs on Windows against fakeProcess. That is the
// requirement, not a convenience: this is the code that decides whether a
// world is saved, and a ladder that can only be exercised on the production
// box is a ladder that gets exercised on the production box.

// fakeControl is a control.Channel-shaped stand-in for the ladder.
type fakeControl struct {
	mu      sync.Mutex
	ready   bool
	unready string
	out     string
	err     error
	got     []string

	// readyFn lets a test wire the PRODUCTION gate closure instead of a
	// constant. A fake that is always ready cannot see a gate that closes at
	// the wrong moment, which is how every control rung came to be skipped.
	readyFn func() bool
}

func (c *fakeControl) Kind() control.Kind { return spec.ControlSourceRCON }

func (c *fakeControl) Close() error { return nil }

func (c *fakeControl) Ready() bool {
	c.mu.Lock()
	fn, ready := c.readyFn, c.ready
	c.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return ready
}

func (c *fakeControl) Unready() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unready
}

func (c *fakeControl) Exec(_ context.Context, cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, cmd)
	return c.out, c.err
}

func (c *fakeControl) sent() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.got...)
}

// ladderKit is one ladder wired to an in-memory console and a fake process.
type ladderKit struct {
	*Ladder
	proc  *fakeProcess
	fake  *console.Fake
	clock *console.FakeClock
	ctrl  *fakeControl
}

func newLadderKit(t *testing.T, steps []PlanStep, tweak ...func(*Ladder)) *ladderKit {
	t.Helper()
	clock := console.NewFakeClock(time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC))
	// PartialAfter negative: these tests only ever emit complete lines, and an
	// idle-flush timer would show up in Timers() and make the clock-advance
	// synchronisation ambiguous.
	f := console.NewFake(console.FakeOptions{Clock: clock, PartialAfter: -1})
	t.Cleanup(func() { _ = f.Close() })

	proc := newFakeProcess()
	ctrl := &fakeControl{ready: true}

	l := &Ladder{
		Plan: Plan{
			Instance: "pz-main",
			Game:     "testgame",
			Unit:     "gvm@pz-main.service",
			Process:  proc.Identity(),
			Steps:    steps,
		},
		Proc:    proc,
		Ring:    f.Ring,
		Console: f,
		Control: ctrl,
		Clock:   clock,
		Log:     slog.New(slog.DiscardHandler),
		Note:    func(format string, a ...any) uint64 { return f.Note(fmt.Sprintf(format, a...)) },
	}
	for _, fn := range tweak {
		fn(l)
	}
	return &ladderKit{Ladder: l, proc: proc, fake: f, clock: clock, ctrl: ctrl}
}

// run starts the ladder on its own goroutine and returns the report channel.
func (k *ladderKit) run(t *testing.T) <-chan *Report {
	t.Helper()
	out := make(chan *Report, 1)
	go func() { out <- k.Run(context.Background(), "user:alice", "test") }()
	return out
}

// settle waits for cond, in REAL time.
//
// It synchronises the test goroutine with the ladder's, which is a different
// job from the fake clock's: the clock decides what the code under test
// believes the time is, and this decides when the test may move it.
func settle(t *testing.T, what string, cond func() bool) {
	t.Helper()
	settleEvery(t, what, 200*time.Microsecond, cond)
}

// settleEvery is settle with the poll interval named.
//
// A predicate that reads a file wants a slower one than a predicate that reads
// memory: writeFileAtomic renames a temp file over the target, and on Windows
// a reader holding the target open makes that rename fail. A tight poll can
// therefore starve the very writer it is waiting for -- a development-machine
// artifact of rename(2) semantics, not a property of the code under test.
func settleEvery(t *testing.T, what string, every time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(every)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (k *ladderKit) waitSent(t *testing.T, n int) {
	t.Helper()
	settle(t, fmt.Sprintf("%d line(s) sent to the game", n), func() bool {
		return len(k.fake.SentLines()) >= n
	})
}

// fire waits for the ladder to arm its wait timer, then expires it.
func (k *ladderKit) fire(t *testing.T, d time.Duration) {
	t.Helper()
	settle(t, "the ladder to arm its wait timer", func() bool { return k.clock.Timers() >= 1 })
	k.clock.Advance(d)
}

func report(t *testing.T, ch <-chan *Report) *Report {
	t.Helper()
	select {
	case rep := <-ch:
		return rep
	case <-time.After(5 * time.Second):
		t.Fatal("the ladder never finished")
		return nil
	}
}

func rungs(rep *Report) []Rung {
	out := make([]Rung, len(rep.Steps))
	for i, s := range rep.Steps {
		out[i] = s.Rung
	}
	return out
}

func hasRung(rep *Report, r Rung) bool {
	for _, s := range rep.Steps {
		if s.Rung == r {
			return true
		}
	}
	return false
}

// pzSteps is Project Zomboid's shipped ladder, which is the shape every
// assertion here is really about.
func pzSteps() []PlanStep {
	return []PlanStep{
		{Via: spec.ViaConsole, Send: "save", WaitForLog: `(?i)\bsave(d|\s+complete|\s+finished)`,
			Wait: MillisOf(30 * time.Second), OnTimeout: "continue"},
		{Via: spec.ViaConsole, Send: "quit", Wait: MillisOf(5 * time.Second)},
		{Via: spec.ViaWaitExit, Wait: MillisOf(240 * time.Second)},
		{Via: spec.ViaSignal, Signal: "SIGTERM", Scope: "process_group", Wait: MillisOf(30 * time.Second)},
	}
}

// TestLadderSaveMarkerQuitExit is the happy path, and it is the one an
// operator's world depends on: save, the game says it saved, quit, the game
// exits. No signal is ever delivered.
func TestLadderSaveMarkerQuitExit(t *testing.T) {
	k := newLadderKit(t, pzSteps())
	ch := k.run(t)

	k.waitSent(t, 1)
	if got := k.fake.SentLines()[0]; got != "save" {
		t.Fatalf("first line sent = %q, want save", got)
	}
	k.fake.Emit("SAVING WORLD... world save complete")

	k.waitSent(t, 2)
	if got := k.fake.SentLines()[1]; got != "quit" {
		t.Fatalf("second line sent = %q, want quit", got)
	}
	k.proc.exit(ExitStatus{Code: 0})

	rep := report(t, ch)
	if rep.FinalRung != RungConsole {
		t.Errorf("final rung = %q, want %q (steps %v)", rep.FinalRung, RungConsole, rungs(rep))
	}
	if rep.Degraded {
		t.Error("a clean save-and-quit must not be degraded")
	}
	if !rep.Exited || rep.ExitCode != 0 {
		t.Errorf("exited=%t code=%d, want a clean exit", rep.Exited, rep.ExitCode)
	}
	if len(k.proc.sent()) != 0 {
		t.Errorf("the game was signalled %v; a ladder that got its marker must not signal", k.proc.sent())
	}
	if len(rep.Steps) != 2 {
		t.Fatalf("steps = %v, want just the two console rungs", rungs(rep))
	}
	save := rep.Steps[0]
	if save.Outcome != OutcomeOK || save.MatchedSeq == 0 {
		t.Errorf("save step = %+v, want ok with a matched marker", save)
	}
	if save.Matched == "" || !strings.Contains(save.Matched, "save complete") {
		t.Errorf("save step matched %q, want the line the game actually printed", save.Matched)
	}
	if rep.Steps[1].Outcome != OutcomeExited {
		t.Errorf("quit step outcome = %q, want %q", rep.Steps[1].Outcome, OutcomeExited)
	}
}

// TestLadderContinuesWithoutTheSaveMarker is section 11.6 rule 2. A `save`
// that produces no marker in 30 s is not a reason to refuse to `quit`: the
// bash suite's blind `sleep 10` already established that a stuck server must
// still make progress, and this reproduces its worst case exactly while
// recording that the marker never came.
func TestLadderContinuesWithoutTheSaveMarker(t *testing.T) {
	k := newLadderKit(t, []PlanStep{
		{Via: spec.ViaConsole, Send: "save", WaitForLog: "(?i)saved", Wait: MillisOf(30 * time.Second)},
		{Via: spec.ViaConsole, Send: "quit"},
	})
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }
	ch := k.run(t)

	k.waitSent(t, 1)
	k.fire(t, 30*time.Second) // the marker never comes

	k.waitSent(t, 2)
	rep := report(t, ch)

	if got := k.fake.SentLines(); len(got) != 2 || got[1] != "quit" {
		t.Fatalf("sent %v, want save then quit", got)
	}
	if rep.Steps[0].Outcome != OutcomeTimeout {
		t.Errorf("save outcome = %q, want %q", rep.Steps[0].Outcome, OutcomeTimeout)
	}
	if rep.Steps[0].Duration.Duration() != 30*time.Second {
		t.Errorf("save took %s; the record must show it waited the full budget", rep.Steps[0].Duration)
	}
	if !hasRung(rep, RungSIGTERM) {
		t.Errorf("rungs %v, want the implicit SIGTERM after a quit that did not end it", rungs(rep))
	}
	if rep.Degraded {
		t.Error("SIGTERM is not degraded; only a SIGKILL is")
	}
}

// TestLadderEscalatesToSIGKILL is the server that will not die. Both signal
// rungs must appear in the record and the report must say degraded, because
// "hung and got SIGKILLed" is exactly what the legacy unit could never
// distinguish from "saved and exited".
func TestLadderEscalatesToSIGKILL(t *testing.T) {
	k := newLadderKit(t, []PlanStep{{Via: spec.ViaConsole, Send: "quit"}})
	ch := k.run(t)

	k.waitSent(t, 1)
	k.fire(t, implicitTermWait) // SIGTERM, ignored
	k.fire(t, implicitKillWait) // SIGKILL to the group, ignored
	k.fire(t, implicitKillWait) // SIGKILL to the cgroup, ignored

	rep := report(t, ch)
	if !rep.Degraded {
		t.Error("a shutdown that reached SIGKILL must be degraded")
	}
	if !hasRung(rep, RungSIGTERM) || !hasRung(rep, RungSIGKILL) {
		t.Fatalf("rungs = %v, want both sigterm and sigkill", rungs(rep))
	}
	want := []recordedSignal{
		{platform.SIGTERM, ScopeGroup},
		{platform.SIGKILL, ScopeGroup},
		{platform.SIGKILL, ScopeCgroup},
	}
	got := k.proc.sent()
	if len(got) != len(want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("signal %d = %v, want %v", i, got[i], want[i])
		}
	}
	if rep.Exited {
		t.Error("the game never exited; the report must not claim it did")
	}
}

// TestLadderSIGKILLIsDegradedEvenWhenItWorks. Reaching a SIGKILL rung means
// the world was not asked to save, it was ended -- whether or not the kill
// landed.
func TestLadderSIGKILLIsDegradedEvenWhenItWorks(t *testing.T) {
	k := newLadderKit(t, nil)
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGKILL }
	ch := k.run(t)

	k.fire(t, implicitTermWait)
	rep := report(t, ch)

	if rep.FinalRung != RungSIGKILL {
		t.Errorf("final rung = %q, want %q", rep.FinalRung, RungSIGKILL)
	}
	if !rep.Degraded {
		t.Error("a SIGKILL that worked is still a degraded shutdown")
	}
	if !rep.Exited {
		t.Error("the game did exit; the report must say so")
	}
}

// TestControlStopDisconnectIsNotAnError is section 11.6 rule 3. `DialectSimple`
// reads until idle; a `quit` that WORKS closes the socket first, so a strict
// reading reports failure on a shutdown that went perfectly -- and a ladder
// that believes its stop step failed escalates to SIGTERM against a server
// that is already saving.
func TestControlStopDisconnectIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"eof", io.EOF},
		{"wrapped eof", fmt.Errorf("rcon: reading reply: %w", io.EOF)},
		{"unexpected eof", io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newLadderKit(t, []PlanStep{
				{Via: spec.ViaControl, Send: "shutdown", TolerateDisconnect: true, Wait: MillisOf(60 * time.Second)},
			})
			k.ctrl.err = tc.err
			ch := k.run(t)

			settle(t, "the control command", func() bool { return len(k.ctrl.sent()) == 1 })
			k.proc.exit(ExitStatus{Code: 0})

			rep := report(t, ch)
			if rep.FinalRung != RungControl {
				t.Errorf("final rung = %q, want %q (steps %v)", rep.FinalRung, RungControl, rungs(rep))
			}
			if rep.Degraded {
				t.Error("a tolerated disconnect is not a degraded shutdown")
			}
			if got := rep.Steps[0].Outcome; got != OutcomeExited {
				t.Errorf("outcome = %q, want %q", got, OutcomeExited)
			}
			if !strings.Contains(rep.Steps[0].Note, "closed the connection") {
				t.Errorf("note = %q, want it to explain the tolerated hangup", rep.Steps[0].Note)
			}
			if len(k.proc.sent()) != 0 {
				t.Errorf("the game was signalled %v after a control stop that worked", k.proc.sent())
			}
		})
	}
}

// TestControlErrorThatIsNotADisconnectFails. The tolerated set is closed on
// purpose: an authentication failure swallowed as "the server hung up" would
// report a shutdown that never reached the server at all.
func TestControlErrorThatIsNotADisconnectFails(t *testing.T) {
	k := newLadderKit(t, []PlanStep{
		{Via: spec.ViaControl, Send: "shutdown", TolerateDisconnect: true},
	})
	k.ctrl.err = errors.New("rcon: authentication failed")
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }
	ch := k.run(t)

	rep := report(t, ch)
	if got := rep.Steps[0].Outcome; got != OutcomeFailed {
		t.Errorf("outcome = %q, want %q", got, OutcomeFailed)
	}
	if !hasRung(rep, RungSIGTERM) {
		t.Errorf("rungs = %v, want the ladder to carry on to the signal rungs", rungs(rep))
	}
}

// TestControlRungsAreSkippedNotFailed. Half the catalogue has no control
// channel and Project Zomboid's is down for the whole first boot; 7 Days to
// Die's ladder deliberately follows every control rung with the same command
// over the console. A skipped rung is what makes that pattern work.
func TestControlRungsAreSkippedNotFailed(t *testing.T) {
	steps := []PlanStep{
		{Via: spec.ViaControl, Send: "saveworld"},
		{Via: spec.ViaConsole, Send: "saveworld"},
		{Via: spec.ViaControl, Send: "shutdown"},
		{Via: spec.ViaConsole, Send: "shutdown"},
	}
	t.Run("no channel at all", func(t *testing.T) {
		k := newLadderKit(t, steps, func(l *Ladder) { l.Control = nil })
		k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }
		rep := report(t, k.run(t))

		if got := k.fake.SentLines(); len(got) != 2 || got[0] != "saveworld" || got[1] != "shutdown" {
			t.Fatalf("console lines = %v, want both console rungs to have run", got)
		}
		if rep.Steps[0].Outcome != OutcomeSkipped {
			t.Errorf("control outcome = %q, want %q", rep.Steps[0].Outcome, OutcomeSkipped)
		}
		if !strings.Contains(rep.Steps[0].Note, "no control channel") {
			t.Errorf("note = %q, want it to name the missing channel", rep.Steps[0].Note)
		}
	})
	t.Run("channel not ready", func(t *testing.T) {
		k := newLadderKit(t, steps)
		k.ctrl.ready = false
		k.ctrl.unready = "RCON does not bind until Project Zomboid finishes booting"
		k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }
		rep := report(t, k.run(t))

		if len(k.ctrl.sent()) != 0 {
			t.Errorf("dialled a channel that is not ready: %v", k.ctrl.sent())
		}
		if got := rep.Steps[0].Note; got != k.ctrl.unready {
			t.Errorf("note = %q, want the channel's own sentence verbatim", got)
		}
	})
}

// TestWaitForLogRejectsALinePredatingTheSend is section 11.6 rule 1, and it is
// the difference between a shutdown that saves and one that does not. Without
// it, a `save` an operator typed an hour ago satisfies the wait guarding
// `quit` instantly, and `quit` lands in the middle of a world write.
func TestWaitForLogRejectsALinePredatingTheSend(t *testing.T) {
	k := newLadderKit(t, []PlanStep{
		{Via: spec.ViaConsole, Send: "save", WaitForLog: "(?i)saved", Wait: MillisOf(30 * time.Second)},
	})
	stale := k.fake.Emit("[19:02:03] world saved")
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }

	ch := k.run(t)
	k.waitSent(t, 1)
	k.fire(t, 30*time.Second)
	rep := report(t, ch)

	save := rep.Steps[0]
	if save.Outcome != OutcomeTimeout {
		t.Fatalf("outcome = %q, want %q: a line printed before the send satisfied the wait", save.Outcome, OutcomeTimeout)
	}
	if save.MatchedSeq != 0 {
		t.Errorf("matched seq %d (%q); the stale line at seq %d must not count",
			save.MatchedSeq, save.Matched, stale)
	}
	if save.SinceSeq <= stale {
		t.Errorf("since seq = %d, must be past the stale line at %d", save.SinceSeq, stale)
	}
}

// TestWaitForLogRejectsTheEchoOfItsOwnCommand is the same bug one line lower
// down. `Send` echoes the command into the ring as SourceActor, and the runner
// annotates the rung before that, so a marker pattern loose enough to match
// the word `save` would be satisfied by gvm's own output before the game had
// done anything at all.
func TestWaitForLogRejectsTheEchoOfItsOwnCommand(t *testing.T) {
	k := newLadderKit(t, []PlanStep{
		{Via: spec.ViaConsole, Send: "save", WaitForLog: "(?i)save", Wait: MillisOf(30 * time.Second)},
	})
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }

	ch := k.run(t)
	k.waitSent(t, 1)
	k.fire(t, 30*time.Second)
	rep := report(t, ch)

	if got := rep.Steps[0].Outcome; got != OutcomeTimeout {
		t.Fatalf("outcome = %q, want %q: the ladder matched its own echo", got, OutcomeTimeout)
	}
	// Prove the ring really did contain matching text the whole time, so the
	// test is asserting the cursor rather than an absent line.
	var matches int
	for _, text := range k.fake.Texts() {
		if strings.Contains(strings.ToLower(text), "save") {
			matches++
		}
	}
	if matches == 0 {
		t.Fatal("the ring held no line matching the pattern; this test proves nothing")
	}
	// Pin the cursor advance itself. LogMarker.Match already refuses anything
	// that is not SourceGame, so the outcome above stays correct even with
	// this line deleted -- which is precisely why it needs its own assertion.
	// Two independent guards protect the same world; a mutation that silently
	// removes one must fail something.
	if st := rep.Steps[0]; st.SinceSeq <= st.EchoSeq {
		t.Errorf("SinceSeq = %d, EchoSeq = %d: the wait window must start after the echo",
			st.SinceSeq, st.EchoSeq)
	}
}

// TestWaitForLogMatchesAFreshLine is the positive half of the two tests above:
// the cursor must not be so strict that a real marker is missed.
func TestWaitForLogMatchesAFreshLine(t *testing.T) {
	k := newLadderKit(t, []PlanStep{
		{Via: spec.ViaConsole, Send: "save", WaitForLog: "(?i)saved", Wait: MillisOf(30 * time.Second)},
	})
	k.fake.Emit("world saved") // stale
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }

	ch := k.run(t)
	k.waitSent(t, 1)
	fresh := k.fake.Emit("world saved")
	rep := report(t, ch)

	if got := rep.Steps[0]; got.Outcome != OutcomeOK || got.MatchedSeq != fresh {
		t.Errorf("step = %+v, want ok matching seq %d", got, fresh)
	}
}

// TestEscalateSkipsTheRemainingDeclaredSteps.
func TestEscalateSkipsTheRemainingDeclaredSteps(t *testing.T) {
	k := newLadderKit(t, []PlanStep{
		{Via: spec.ViaConsole, Send: "save", WaitForLog: "(?i)saved",
			Wait: MillisOf(10 * time.Second), OnTimeout: OnTimeoutEscalate},
		{Via: spec.ViaConsole, Send: "quit"},
	})
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }

	ch := k.run(t)
	k.waitSent(t, 1)
	k.fire(t, 10*time.Second)
	rep := report(t, ch)

	if got := k.fake.SentLines(); len(got) != 1 || got[0] != "save" {
		t.Errorf("sent %v, want the ladder to stop after the escalating step", got)
	}
	if !hasRung(rep, RungSIGTERM) {
		t.Errorf("rungs = %v, want it to jump to the signal rungs", rungs(rep))
	}
}

// TestSignalRungGoneCountsAsSuccess. ErrGone means there was nothing left to
// signal, which is what the rung was asked to achieve. Reading it as a failure
// would escalate to SIGKILL against a world that had already saved and exited.
func TestSignalRungGoneCountsAsSuccess(t *testing.T) {
	k := newLadderKit(t, []PlanStep{
		{Via: spec.ViaSignal, Signal: "SIGINT", Scope: "process_group", Wait: MillisOf(90 * time.Second)},
	})
	k.proc.alive = false
	k.proc.empty = true
	rep := report(t, k.run(t))

	if rep.FinalRung != RungNone {
		t.Errorf("final rung = %q, want %q for a game that had already exited", rep.FinalRung, RungNone)
	}
	if rep.Degraded {
		t.Error("a game that exited on its own is not a degraded shutdown")
	}
	if len(rep.Steps) != 0 {
		t.Errorf("steps = %v, want none: there was nothing to do", rungs(rep))
	}
}

// TestSignalRungFailureEscalates. A delivery that errored is a rung that did
// NOT work, and waiting out its budget for an exit that is not coming wastes
// the stop budget the next rung needs.
func TestSignalRungFailureEscalates(t *testing.T) {
	k := newLadderKit(t, []PlanStep{
		{Via: spec.ViaSignal, Signal: "SIGINT", Scope: "process_group", Wait: MillisOf(90 * time.Second)},
	})
	// Every delivery fails, so no rung ever gets as far as arming a wait: the
	// ladder must walk straight through them rather than sitting out four
	// budgets for exits that were never coming.
	k.proc.signalErr = errors.New("operation not permitted")
	rep := report(t, k.run(t))

	if rep.Steps[0].Outcome != OutcomeFailed {
		t.Errorf("outcome = %q, want %q", rep.Steps[0].Outcome, OutcomeFailed)
	}
	if !hasRung(rep, RungSIGKILL) {
		t.Errorf("rungs = %v, want escalation after a failed signal", rungs(rep))
	}
}

// orphanedEngine models the failure this whole subsystem exists for, and the
// reason section 1.2's second bug is called out by name: PZ's start-server.sh
// launches ProjectZomboid64 in a subshell without exec, so the pid gvm pinned
// is the SHELL. The shell dies to SIGTERM and the JVM it forked keeps running
// -- and keeps writing to the world.
type orphanedEngine struct {
	*fakeProcess
	mu    sync.Mutex
	swept bool
}

func (o *orphanedEngine) Empty() (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.swept, nil
}

func (o *orphanedEngine) Signal(sig platform.Signal, scope Scope) error {
	if scope != ScopeCgroup {
		return o.fakeProcess.Signal(sig, scope)
	}
	o.mu.Lock()
	first := !o.swept
	o.swept = true
	o.mu.Unlock()

	o.fakeProcess.mu.Lock()
	o.fakeProcess.signals = append(o.fakeProcess.signals, recordedSignal{sig, scope})
	o.fakeProcess.mu.Unlock()
	if !first {
		return ErrGone
	}
	return nil
}

// TestCgroupSweepReachesAnEngineThatOutlivedItsLeader. The pinned process is
// gone and the group signal reports nothing left, yet the world is still being
// written. Only the cgroup sweep can see that, and stopping at the group would
// leave a live server with no supervisor.
func TestCgroupSweepReachesAnEngineThatOutlivedItsLeader(t *testing.T) {
	var orphan *orphanedEngine
	k := newLadderKit(t, nil, func(l *Ladder) {
		orphan = &orphanedEngine{fakeProcess: l.Proc.(*fakeProcess)}
		l.Proc = orphan
	})
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }

	rep := report(t, k.run(t))

	want := []recordedSignal{
		{platform.SIGTERM, ScopeGroup},
		{platform.SIGKILL, ScopeGroup},
		{platform.SIGKILL, ScopeCgroup},
	}
	got := k.proc.sent()
	if len(got) != len(want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("signal %d = %v, want %v", i, got[i], want[i])
		}
	}
	if rep.FinalRung != RungCgroup {
		t.Errorf("final rung = %q, want %q", rep.FinalRung, RungCgroup)
	}
	if !rep.Degraded {
		t.Error("a sweep is a SIGKILL, and a SIGKILL is degraded")
	}
}

// TestCgroupSweepIsRefusedWhenItCannotBeProved. Outside a service cgroup the
// pid list is an operator's login session, and "SIGKILL everything left" is a
// way to log them out of their own box. ErrNoCgroup must never produce a pid
// list, and the ladder must say so rather than stopping silently.
func TestCgroupSweepIsRefusedWhenItCannotBeProved(t *testing.T) {
	k := newLadderKit(t, nil)
	k.proc.emptyErr = ErrNoCgroup
	// The leader dies to SIGKILL but the sweep can never be proven safe.
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGKILL }

	ch := k.run(t)
	k.fire(t, implicitTermWait)
	rep := report(t, ch)

	for _, s := range k.proc.sent() {
		if s.Scope == ScopeCgroup {
			t.Fatalf("swept a cgroup gvm cannot prove it owns: %v", k.proc.sent())
		}
	}
	if rep.FinalRung != RungSIGKILL {
		t.Errorf("final rung = %q, want %q", rep.FinalRung, RungSIGKILL)
	}
	if !rep.Degraded {
		t.Error("SIGKILL is degraded")
	}
	last := rep.Steps[len(rep.Steps)-1]
	if !strings.Contains(last.Note, "cannot see into the cgroup") {
		t.Errorf("note = %q, want it to say what gvm could not prove", last.Note)
	}
}

// TestALadderStopsWhenTheCgroupIsUnreadableAndTheLeaderIsGone. "Cannot tell"
// must never become a pid list, and it must never become silence either.
func TestALadderStopsWhenTheCgroupIsUnreadableAndTheLeaderIsGone(t *testing.T) {
	k := newLadderKit(t, pzSteps())
	k.proc.alive = false
	k.proc.emptyErr = ErrNoCgroup
	rep := report(t, k.run(t))

	if len(k.proc.sent()) != 0 {
		t.Errorf("signalled %v with no proof there was anything to signal", k.proc.sent())
	}
	if rep.FinalRung != RungNone {
		t.Errorf("final rung = %q, want %q", rep.FinalRung, RungNone)
	}
	var said bool
	for _, text := range k.fake.Texts() {
		if strings.Contains(text, "cannot see into the cgroup") {
			said = true
		}
	}
	if !said {
		t.Errorf("console = %v, want the limit stated in the log the operator is reading", k.fake.Texts())
	}
}

// TestLadderReportsAnAlreadyExitedGame. A game that crashed two seconds before
// the operator pressed stop is not a degraded shutdown.
func TestLadderReportsAnAlreadyExitedGame(t *testing.T) {
	k := newLadderKit(t, pzSteps())
	k.proc.exit(ExitStatus{Code: 1})
	rep := report(t, k.run(t))

	if rep.FinalRung != RungNone || rep.Degraded {
		t.Errorf("final rung = %q degraded = %t, want %q and false", rep.FinalRung, rep.Degraded, RungNone)
	}
	if len(k.fake.SentLines()) != 0 {
		t.Errorf("sent %v to a game that had already exited", k.fake.SentLines())
	}
	if !rep.Exited || rep.ExitCode != 1 {
		t.Errorf("exited=%t code=%d, want the child's real status", rep.Exited, rep.ExitCode)
	}
}

// TestReportCarriesTheConsoleTail is section 11.6 rule 8, mirroring the bash
// suite's `tail -n 30` on early exit. It is the thing you actually want at
// 2am, and it has to be in the record because by the time anyone reads it the
// ring is gone with the process.
func TestReportCarriesTheConsoleTail(t *testing.T) {
	k := newLadderKit(t, []PlanStep{{Via: spec.ViaConsole, Send: "quit"}})
	for i := range 50 {
		k.fake.Emitf("boot line %d", i)
	}
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }
	rep := report(t, k.run(t))

	if len(rep.ConsoleTail) != ConsoleTailLines {
		t.Fatalf("tail has %d lines, want %d", len(rep.ConsoleTail), ConsoleTailLines)
	}
	var sawEcho bool
	for _, l := range rep.ConsoleTail {
		if l == "> quit" {
			sawEcho = true
		}
	}
	if !sawEcho {
		t.Errorf("tail = %v, want the operator's own line marked with '>'", rep.ConsoleTail)
	}
}

// TestDeclaredStepsAreCutOffByTheBudget. `budget` becomes TimeoutStopSec, so a
// ladder that spends all of it on declared steps gets SIGKILLed by systemd
// mid-save with no report written -- the one outcome worse than a degraded
// stop.
func TestDeclaredStepsAreCutOffByTheBudget(t *testing.T) {
	k := newLadderKit(t, []PlanStep{
		{Via: spec.ViaConsole, Send: "save", WaitForLog: "(?i)saved", Wait: MillisOf(300 * time.Second)},
		{Via: spec.ViaConsole, Send: "quit", Wait: MillisOf(300 * time.Second)},
	})
	k.Plan.Budget = MillisOf(120 * time.Second)
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }

	ch := k.run(t)
	k.waitSent(t, 1)
	// The first step's 300 s wait must have been clamped to what is left after
	// the reserve, so this expires it.
	k.fire(t, 120*time.Second-reportReserve)
	rep := report(t, ch)

	save := rep.Steps[0]
	if save.Duration.Duration() >= 300*time.Second {
		t.Errorf("save waited %s; the budget must clamp it", save.Duration)
	}
	if !strings.Contains(save.Note, "budget") {
		t.Errorf("note = %q, want it to say the budget cut the wait", save.Note)
	}
	if rep.Steps[1].Outcome != OutcomeSkipped {
		t.Errorf("second step = %q, want it skipped once the budget was spent", rep.Steps[1].Outcome)
	}
}

// TestConcurrentShutdownsAreRefused. Two ladders on one world would race a
// save against a SIGTERM.
func TestConcurrentShutdownsAreRefused(t *testing.T) {
	h := newHarness(t)
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	h.stopping.Store(true)
	rep := h.Shutdown(context.Background(), "user:alice", "test")
	if rep.Err == "" || !strings.Contains(rep.Err, "already in progress") {
		t.Errorf("report err = %q, want a refusal", rep.Err)
	}
	if len(h.proc.sent()) != 0 {
		t.Errorf("the second ladder signalled the game: %v", h.proc.sent())
	}
}

func TestBuildPlan(t *testing.T) {
	t.Run("renders the shipped shape", func(t *testing.T) {
		m := testManifest()
		m.Spec.Verbs = map[string]spec.Verb{"stop": {Control: "DoExit"}}
		m.Spec.Shutdown.Steps = []spec.ShutdownStep{
			{Via: spec.ViaControl, Send: "DoExit", Wait: spec.Duration(15 * time.Second)},
			{Via: spec.ViaControl, Send: "Save", Wait: spec.Duration(45 * time.Second)},
			{Via: spec.ViaSignal, Signal: "SIGTERM", Wait: spec.Duration(20 * time.Second)},
		}
		steps, err := BuildPlan(m, testRenderer())
		if err != nil {
			t.Fatal(err)
		}
		if !steps[0].TolerateDisconnect {
			t.Error("a control step sending verbs.stop.control must tolerate a disconnect by default")
		}
		if steps[1].TolerateDisconnect {
			t.Error("`Save` is not the stop command; it must not tolerate a disconnect")
		}
		// An omitted scope is the process GROUP, which is the whole lesson of
		// the launcher-script-without-exec bug.
		if steps[2].Scope != ScopeGroup.String() {
			t.Errorf("scope = %q, want %q", steps[2].Scope, ScopeGroup)
		}
	})

	t.Run("refuses a secret in a shutdown command", func(t *testing.T) {
		m := testManifest()
		m.Spec.Shutdown.Steps = []spec.ShutdownStep{
			{Via: spec.ViaConsole, Send: "login {{ .Secret.admin }}"},
		}
		_, err := BuildPlan(m, testRenderer())
		if err == nil || !strings.Contains(err.Error(), "secret") {
			t.Fatalf("err = %v, want a refusal naming the secret", err)
		}
		if strings.Contains(fmt.Sprint(err), "hunter2") {
			t.Fatal("the refusal echoed the secret")
		}
	})

	for _, tc := range []struct {
		name string
		step spec.ShutdownStep
		want string
	}{
		{"unknown via", spec.ShutdownStep{Via: "telnet"}, "not a shutdown channel"},
		{"unknown signal", spec.ShutdownStep{Via: spec.ViaSignal, Signal: "SIGPWR"}, "not a signal"},
		{"cgroup scope", spec.ShutdownStep{Via: spec.ViaSignal, Signal: "SIGTERM", Scope: "cgroup"}, "not a shutdown scope"},
		{"console with no send", spec.ShutdownStep{Via: spec.ViaConsole}, "needs a `send`"},
		{"bad marker", spec.ShutdownStep{Via: spec.ViaConsole, Send: "save", WaitForLog: "("}, "wait_for_log"},
		{"bad on_timeout", spec.ShutdownStep{Via: spec.ViaConsole, Send: "save", OnTimeout: "abort"}, "continue|escalate"},
		{"line break in send", spec.ShutdownStep{Via: spec.ViaConsole, Send: "save\nop attacker"}, "send"},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			m := testManifest()
			m.Spec.Shutdown.Steps = []spec.ShutdownStep{tc.step}
			_, err := BuildPlan(m, testRenderer())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// TestBuildPlanRefusalStopsTheServerFromStarting. Rendering the ladder at
// construction is what turns "this world cannot be stopped safely" into a
// refusal to start it.
func TestBuildPlanRefusalStopsTheServerFromStarting(t *testing.T) {
	m := testManifest()
	m.Spec.Shutdown.Steps = []spec.ShutdownStep{{Via: spec.ViaSignal, Signal: "SIGPWR"}}
	_, err := New(Config{
		Instance: "pz-main", Game: "testgame",
		Paths: testPaths(t), Manifest: m, Renderer: testRenderer(),
		BaseEnv: []string{"PATH=/usr/bin"},
	})
	if err == nil || !strings.Contains(err.Error(), "not a signal") {
		t.Fatalf("New err = %v, want the ladder refusal", err)
	}
}

func TestDisconnected(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{io.EOF, true},
		{fmt.Errorf("wrapped: %w", io.EOF), true},
		{io.ErrUnexpectedEOF, true},
		{errors.New("authentication failed"), false},
		{context.DeadlineExceeded, false},
	} {
		if got := disconnected(tc.err); got != tc.want {
			t.Errorf("disconnected(%v) = %t, want %t", tc.err, got, tc.want)
		}
	}
}

func TestPlanRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/shutdown.json"
	want := Plan{
		Instance: "pz-main", Game: "projectzomboid", Unit: "gvm@pz-main.service",
		Process:    Identity{PID: 41799, PGID: 41799, StartTicks: 8391244},
		ConsoleFD:  "/proc/41799/fd/0",
		LineEnding: "\r",
		Budget:     MillisOf(360 * time.Second),
		Steps:      pzSteps(),
	}
	if err := WritePlan(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPlan(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Proto != PlanProto {
		t.Errorf("proto = %d, want %d", got.Proto, PlanProto)
	}
	if len(got.Steps) != len(want.Steps) || got.Steps[0].WaitForLog != want.Steps[0].WaitForLog {
		t.Errorf("steps = %+v, want %+v", got.Steps, want.Steps)
	}
	if got.LineEnding != "\r" {
		t.Errorf("line ending = %q; a game that needs CR and silently gets LF accepts nothing", got.LineEnding)
	}
}

// TestImplicitRungsAreClampedByTheBudgetToo. Once the budget is spent the
// signals are still DELIVERED -- all three of them -- because waiting past
// TimeoutStopSec for one to take effect only means systemd kills the runner
// before it can say what it did.
func TestImplicitRungsAreClampedByTheBudgetToo(t *testing.T) {
	k := newLadderKit(t, []PlanStep{
		{Via: spec.ViaWaitExit, Wait: MillisOf(60 * time.Second)},
	})
	k.Plan.Budget = MillisOf(10 * time.Second)

	ch := k.run(t)
	// The declared wait was clamped to the budget minus the report reserve, so
	// this is the only clock move the whole ladder needs.
	k.fire(t, 10*time.Second-reportReserve)
	rep := report(t, ch)

	want := []recordedSignal{
		{platform.SIGTERM, ScopeGroup},
		{platform.SIGKILL, ScopeGroup},
		{platform.SIGKILL, ScopeCgroup},
	}
	got := k.proc.sent()
	if len(got) != len(want) {
		t.Fatalf("signals = %v, want all three delivered even with no budget left", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("signal %d = %v, want %v", i, got[i], want[i])
		}
	}
	if !rep.Degraded {
		t.Error("the game outlived every rung; that is degraded")
	}
	if rep.Duration.Duration() > 10*time.Second {
		t.Errorf("the ladder ran %s past a %s budget", rep.Duration, k.Plan.Budget)
	}
}

// TestRunnerShutdownWritesTheRecord is the runner-level integration: the ladder
// runs against the spawned process, the report reaches both files section 11.6
// rule 9 names, and an attached client is narrated each rung as it happens.
func TestRunnerShutdownWritesTheRecord(t *testing.T) {
	h := newHarness(t)
	h.cfg.Paths.ShutdownLog = filepath.Join(h.cfg.Paths.Logs, "shutdown.jsonl")
	h.cfg.Paths.LastShutdown = filepath.Join(h.cfg.Paths.Run, "last-shutdown.json")
	h.cfg.Manifest.Spec.Shutdown.Steps = []spec.ShutdownStep{
		{Via: spec.ViaConsole, Send: "quit"},
	}
	steps, err := BuildPlan(h.cfg.Manifest, h.cfg.Renderer)
	if err != nil {
		t.Fatal(err)
	}
	h.steps = steps
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	h.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }

	var narrated []string
	var mu sync.Mutex
	raw, err := h.Stop(context.Background(), "user:alice", "operator pressed stop",
		func(rung, send string) {
			mu.Lock()
			narrated = append(narrated, rung+":"+send)
			mu.Unlock()
		})
	if err != nil {
		t.Fatal(err)
	}

	var rep Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Instance != "pz-main" || rep.Actor != "user:alice" {
		t.Errorf("report = %+v", rep)
	}
	if !strings.Contains(h.stdin.String(), "quit") {
		t.Errorf("stdin = %q, want the quit that the ladder sent", h.stdin.String())
	}
	mu.Lock()
	got := append([]string(nil), narrated...)
	mu.Unlock()
	want := []string{"console:quit", "sigterm:SIGTERM"}
	if len(got) != len(want) {
		t.Fatalf("narration = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("narration[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if _, err := ReadLastShutdown(h.cfg.Paths.LastShutdown); err != nil {
		t.Errorf("last-shutdown.json: %v", err)
	}
	if _, err := os.Stat(h.cfg.Paths.ShutdownLog); err != nil {
		t.Errorf("the journal gvmd ingests: %v", err)
	}
	if got := h.State(); got != StateExited {
		t.Errorf("state = %q, want %q", got, StateExited)
	}
}

// TestControlRungsSurviveTheStoppingTransition is Palworld's world.
//
// Runner.Shutdown moves the runner to `stopping` BEFORE it builds the ladder,
// so a control gate that closes at `ready` skips every `via: control` rung.
// Palworld's shipped ladder is `Save` then `DoExit` over RCON with no console
// equivalent, so both saves would be recorded as skipped and the world would
// fall through to signals -- with `degraded: false` in the report, because only
// the SIGKILL rungs set it.
//
// The gate is wired here exactly as `gvm run` wires it, because a fake that is
// always ready is precisely the thing that could not see this.
func TestControlRungsSurviveTheStoppingTransition(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		m := testManifest()
		m.Spec.Shutdown.Steps = []spec.ShutdownStep{
			{Via: spec.ViaControl, Send: "Save"},
			{Via: spec.ViaSignal, Signal: "SIGTERM", Scope: "process_group"},
		}
		c.Manifest = m
	})
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	// Spawn leaves the runner in `booting`.
	if !h.SetState(StateReady) {
		t.Fatal("the runner never reached ready")
	}

	ctrl := &fakeControl{unready: "RCON does not bind until the server has booted"}
	ctrl.readyFn = h.ControlGateOpen
	h.SetControl(ctrl)
	h.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }

	rep := h.Shutdown(context.Background(), "user:alice", "test")

	if got := ctrl.sent(); len(got) != 1 || got[0] != "Save" {
		t.Fatalf("control commands executed = %v, want [Save]; the world was never asked to save", got)
	}
	if rep.Steps[0].Outcome == OutcomeSkipped {
		t.Errorf("the save rung was skipped: %q", rep.Steps[0].Note)
	}
}

// TestTheControlGateStaysShutForAStopDuringBoot is the other half of the rule.
//
// A stop that arrives during Project Zomboid's forty minute boot never had an
// RCON socket to talk to. Opening the gate for it would turn an honest
// OutcomeSkipped carrying the manifest's not_ready_reason into an
// OutcomeFailed carrying a connection-refused string.
func TestTheControlGateStaysShutForAStopDuringBoot(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		m := testManifest()
		m.Spec.Shutdown.Steps = []spec.ShutdownStep{
			{Via: spec.ViaControl, Send: "Save"},
			{Via: spec.ViaSignal, Signal: "SIGTERM", Scope: "process_group"},
		}
		c.Manifest = m
	})
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}
	if h.State() != StateBooting {
		t.Fatalf("state after spawn = %s, want booting", h.State())
	}

	ctrl := &fakeControl{unready: "RCON does not bind until the server has booted"}
	ctrl.readyFn = h.ControlGateOpen
	h.SetControl(ctrl)
	h.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }

	rep := h.Shutdown(context.Background(), "user:alice", "test")

	if got := ctrl.sent(); len(got) != 0 {
		t.Fatalf("control commands executed = %v; RCON was never bound on this boot", got)
	}
	if rep.Steps[0].Outcome != OutcomeSkipped {
		t.Errorf("save rung outcome = %q, want %q", rep.Steps[0].Outcome, OutcomeSkipped)
	}
	if !strings.Contains(rep.Steps[0].Note, "RCON does not bind") {
		t.Errorf("note = %q, want the manifest's not_ready_reason", rep.Steps[0].Note)
	}
}

// TestControlGateOpen pins the rule itself, which is the one `gvm run` hands
// the RCON client.
func TestControlGateOpen(t *testing.T) {
	// A stop that arrives during the boot. `booting -> stopping` is a legal
	// transition and the port was never bound, so the gate must stay shut.
	h := newHarness(t)
	for _, s := range []State{StateSpawning, StateBooting, StateStopping} {
		if h.ControlGateOpen() {
			t.Errorf("the gate is open in %s; nothing has bound the port yet", h.State())
		}
		if !h.SetState(s) {
			t.Fatalf("could not move to %s", s)
		}
	}
	if h.ControlGateOpen() {
		t.Error("the gate is open for a stop that arrived during the boot")
	}

	h2 := newHarness(t)
	for _, s := range []State{StateSpawning, StateBooting, StateReady} {
		if !h2.SetState(s) {
			t.Fatalf("could not move to %s", s)
		}
	}
	if !h2.ControlGateOpen() {
		t.Error("the gate is shut on a ready server")
	}
	if !h2.SetState(StateStopping) {
		t.Fatal("could not move to stopping")
	}
	if !h2.ControlGateOpen() {
		t.Error("the gate shut on the stopping transition; every control rung would be skipped")
	}
	if !h2.SetState(StateExited) {
		t.Fatal("could not move to exited")
	}
	if h2.ControlGateOpen() {
		t.Error("the gate is open after the game exited")
	}
}

// TestACancelledContextAbandonsTheLadderRatherThanRacingToSIGKILL.
//
// Ctrl-C at the terminal cancels the context the ladder is waiting on. Every
// remaining rung then delivers its side effect and its wait returns instantly
// on the closed Done channel, so Project Zomboid's ~335 s ladder collapses into
// microseconds and the implicit SIGKILL lands on a JVM in the middle of a world
// write. Declining to finish is the safe answer: a world that is still running
// can be stopped again, a killed one cannot be un-killed.
func TestACancelledContextAbandonsTheLadderRatherThanRacingToSIGKILL(t *testing.T) {
	k := newLadderKit(t, pzSteps())
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan *Report, 1)
	go func() { out <- k.Run(ctx, "user:alice", "test") }()

	k.waitSent(t, 1)
	settle(t, "the ladder to arm its wait timer", func() bool { return k.clock.Timers() >= 1 })
	cancel()

	rep := report(t, out)
	for _, s := range k.proc.sent() {
		if s.Sig == platform.SIGKILL {
			t.Fatalf("the abandoned ladder still delivered SIGKILL: %v", k.proc.sent())
		}
	}
	if len(k.fake.SentLines()) != 1 {
		t.Errorf("lines sent = %q; an abandoned ladder must not keep sending", k.fake.SentLines())
	}
	if !rep.Degraded {
		t.Error("an abandoned shutdown is degraded; the game is still running")
	}
	if rep.Err == "" || !strings.Contains(rep.Err, "abandoned") {
		t.Errorf("report err = %q, want it to say the shutdown was abandoned", rep.Err)
	}
	if hasRung(rep, RungSIGTERM) || hasRung(rep, RungSIGKILL) || hasRung(rep, RungCgroup) {
		t.Errorf("the implicit rungs ran anyway: %v", rungs(rep))
	}
}

// TestWaitForLogIgnoresAConcurrentActorLine is the same stale-match bug from a
// third direction: not a line from an hour ago, and not gvm's own echo of the
// command, but a line an operator typed WHILE the save was running.
//
// The console socket keeps accepting `send` for the whole ladder --
// deliberately, because taking the operator's window away during the save they
// are waiting on would be worse. Project Zomboid's shipped marker is
// `(?i)\bsave(d|\s+complete|\s+finished)`, which `servermsg "world saved,
// server restarting"` satisfies, and matching it releases `quit` onto a world
// whose save has seconds left to run.
func TestWaitForLogIgnoresAConcurrentActorLine(t *testing.T) {
	k := newLadderKit(t, []PlanStep{
		{Via: spec.ViaConsole, Send: "save",
			WaitForLog: `(?i)\bsave(d|\s+complete|\s+finished)`, Wait: MillisOf(30 * time.Second)},
	})
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }

	ch := k.run(t)
	k.waitSent(t, 1)
	// The marker has to be watching before the decoys land, or the test proves
	// only that the ladder had not looked yet.
	settle(t, "the ladder to arm its wait timer", func() bool { return k.clock.Timers() >= 1 })
	settle(t, "the marker to subscribe", func() bool { return k.fake.Ring.Subscribers() >= 1 })

	decoy, err := k.fake.Send("user:alice", `servermsg "world saved, server restarting"`)
	if err != nil {
		t.Fatal(err)
	}
	k.fake.Note("-- gvm: the world was saved")

	// The real marker, from the game itself. The ladder must be released by
	// THIS line and no earlier one.
	real := k.fake.Emit("SAVING WORLD... world save complete")
	rep := report(t, ch)

	step := rep.Steps[0]
	if step.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q, want %q", step.Outcome, OutcomeOK)
	}
	if step.MatchedSeq == decoy {
		t.Fatalf("the ladder matched an operator's own line (%q); only the game's output can prove a save finished",
			step.Matched)
	}
	if step.MatchedSeq != real {
		t.Errorf("matched seq %d (%q), want the game's line at %d", step.MatchedSeq, step.Matched, real)
	}
}

// TestAwaitStopBlocksUntilTheWinnersReportIsOnDisk.
//
// A socket-initiated `stop` and `gvm run`'s own teardown are two goroutines
// with no barrier between them: osProcess.Wait is a closed-channel broadcast,
// so the ladder's own successful `quit` wakes supervise in the same instant as
// the ladder's exit watcher. Whoever loses that race tears the runner down --
// closing the ring the ladder is annotating and dropping the connection the
// operator is watching -- while the winner still has settled(), the reap, the
// state write and rep.Save (which fsyncs) left to do.
func TestAwaitStopBlocksUntilTheWinnersReportIsOnDisk(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		m := testManifest()
		m.Spec.Shutdown.Steps = []spec.ShutdownStep{
			{Via: spec.ViaConsole, Send: "quit", Wait: spec.Duration(30 * time.Second)},
		}
		c.Manifest = m
		c.Paths.ShutdownLog = filepath.Join(c.Paths.Logs, "shutdown.jsonl")
		c.Paths.LastShutdown = filepath.Join(c.Paths.Run, "last-shutdown.json")
	})
	if err := h.Spawn(); err != nil {
		t.Fatal(err)
	}

	// Nothing running: AwaitStop is not allowed to block a teardown that has
	// no ladder to wait for.
	if rep := h.AwaitStop(context.Background()); rep != nil {
		t.Fatalf("AwaitStop returned %+v with no ladder running", rep)
	}

	winner := make(chan *Report, 1)
	go func() { winner <- h.Shutdown(context.Background(), "user:alice", "operator stop") }()
	settle(t, "the ladder to start", func() bool { return h.Stopping() })

	awaited := make(chan *Report, 1)
	go func() { awaited <- h.AwaitStop(context.Background()) }()

	select {
	case rep := <-awaited:
		t.Fatalf("AwaitStop returned %+v while the ladder was still running", rep)
	case <-time.After(50 * time.Millisecond):
	}

	h.proc.exit(ExitStatus{Code: 0})
	got := report(t, awaited)
	if got == nil {
		t.Fatal("AwaitStop returned no report for a ladder it waited on")
	}
	if want := <-winner; got != want {
		t.Errorf("AwaitStop returned a different report than the ladder produced")
	}
	// The record is the product: the wait exists so the process cannot exit
	// between the ladder finishing and rep.Save returning.
	if _, err := ReadLastShutdown(h.cfg.Paths.LastShutdown); err != nil {
		t.Errorf("the report was not on disk when AwaitStop returned: %v", err)
	}
	if rep := h.AwaitStop(context.Background()); rep != nil {
		t.Errorf("AwaitStop still reports a ladder in progress: %+v", rep)
	}
}

// TestADeclaredSIGKILLIsDegradedToo.
//
// Degradation is keyed on the SIGNAL, not on gvm having escalated past the
// declared ladder: the implicit SIGTERM is not degraded and a SIGKILL that
// worked still is. A manifest can name SIGKILL -- spec's knownSignals permits
// it and games/generic-steam.yaml already ships a trailing `{via: signal,
// signal: SIGTERM}` that is one word away from it -- and runRungs returns on
// !res.Alive before runImplicit, so the only two writers of rep.Degraded never
// ran. `degraded` is the field gvmd indexes and alerts on, and it said the
// hard-killed world was fine.
func TestADeclaredSIGKILLIsDegradedToo(t *testing.T) {
	k := newLadderKit(t, []PlanStep{
		{Via: spec.ViaSignal, Signal: "SIGKILL", Scope: ScopeGroup.String(), Wait: MillisOf(20 * time.Second)},
	})
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGKILL }

	rep := report(t, k.run(t))
	if !rep.Degraded {
		t.Error("a declared SIGKILL reported degraded=false; the world was ended, not stopped")
	}
	if rep.FinalRung != RungSignal {
		t.Errorf("final rung = %q, want %q: the rung name is still the declared one", rep.FinalRung, RungSignal)
	}
}

// The mirror image, and the one that keeps the rule honest: a declared SIGTERM
// is a clean stop. Only a hard kill degrades.
func TestADeclaredSIGTERMIsNotDegraded(t *testing.T) {
	k := newLadderKit(t, []PlanStep{
		{Via: spec.ViaSignal, Signal: "SIGTERM", Scope: ScopeGroup.String(), Wait: MillisOf(20 * time.Second)},
	})
	k.proc.exitOn = func(sig platform.Signal, _ Scope) bool { return sig == platform.SIGTERM }

	rep := report(t, k.run(t))
	if rep.Degraded {
		t.Error("a declared SIGTERM was reported degraded")
	}
}

// The implicit ladder reads degradation off the rung NAME and a declared step
// reads it off the signal, because a declared step's rung is always "signal".
// Two spellings of one rule, pinned together here so they cannot drift: a
// world that was ended rather than stopped is degraded either way.
func TestTheTwoDegradationRulesAgree(t *testing.T) {
	for _, c := range []struct {
		rung Rung
		sig  platform.Signal
	}{
		{RungSIGTERM, platform.SIGTERM},
		{RungSIGKILL, platform.SIGKILL},
		{RungCgroup, platform.SIGKILL},
	} {
		byName := c.rung.Degrades()
		bySignal := degradesBySignal(PlanStep{Via: spec.ViaSignal, Signal: c.sig.String()})
		if byName != bySignal {
			t.Errorf("%s: Degrades()=%t but degradesBySignal(%s)=%t", c.rung, byName, c.sig, bySignal)
		}
	}
	// A step that sends no signal at all cannot degrade by this rule.
	for _, ps := range []PlanStep{
		{Via: spec.ViaConsole, Send: "quit"},
		{Via: spec.ViaControl, Send: "DoExit"},
		{Via: spec.ViaWaitExit},
		{Via: spec.ViaSignal, Signal: "not a signal"},
	} {
		if degradesBySignal(ps) {
			t.Errorf("%+v was reported as degrading", ps)
		}
	}
}
