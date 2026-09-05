package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/control"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/probe"
	"github.com/S95F/gameVPSmanager/internal/rcon"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// This file is the most safety-critical code in gvm.
//
// Killing a Project Zomboid server mid-write is the standard way to corrupt a
// world, and the legacy unit only avoids it by accident: `KillSignal=SIGCONT`
// neuters systemd's terminate step, and `ExecStop=screen -X stuff 'quit\015'`
// returns immediately, so systemd's TimeoutStopSec is the only thing actually
// waiting. It works, and it cannot tell you whether it worked (section 1.2).
//
// gvm has no ExecStop at all. The runner is the unit's main process, this
// ladder is the only code permitted to signal the game, and it BLOCKS until
// the process group is empty. Every rung it attempted, and what that rung
// achieved, is written down.
//
// The ladder is declarative: the manifest's shutdown.steps, rendered at spawn
// into a Plan. Rendering at spawn rather than at stop is deliberate. It is the
// same bytes the standalone `gvm run-shutdown` path reads (section 11.8 layer
// 3), and a manifest that cannot be rendered fails while the world is still
// safely running rather than at the moment somebody needs it stopped.

const (
	// implicitTermWait is how long the implicit SIGTERM rung waits before
	// escalating (section 11.6 rule 7).
	implicitTermWait = 30 * time.Second
	// implicitKillWait is how long the implicit SIGKILL rung waits for the
	// kernel to finish tearing the process down. SIGKILL is not instantaneous:
	// a process in uninterruptible sleep stays visible until its I/O returns,
	// and sweeping the cgroup before the group kill has landed would report a
	// survivor that was already dying.
	implicitKillWait = 10 * time.Second
	// reportReserve is the slice of the manifest's budget the ladder keeps back
	// for writing the report. `budget` becomes TimeoutStopSec, so a ladder that
	// spends the last millisecond of it is a ladder systemd SIGKILLs before the
	// record reaches disk -- and a shutdown nobody can audit is the one outcome
	// worse than a degraded one.
	//
	// It is deliberately SMALL. The obvious alternative, reserving enough for
	// the implicit rungs as well, would cut a declared `save` short on four of
	// the six shipped manifests -- whose declared waits are upper bounds, not
	// costs, and whose whole point is to save the world before anything gets
	// signalled.
	reportReserve = 2 * time.Second
)

// PlanProto is the schema version of shutdown.json.
const PlanProto = 1

// PlanStep is one rung of the ladder, fully rendered.
//
// There are no templates left in it, and that is the point: `gvm run-shutdown`
// executes this file with no manifest loading, no renderer and no spec
// package, so a runner that has died cannot take the ability to stop the world
// with it.
type PlanStep struct {
	Via        string `json:"via"`
	Send       string `json:"send,omitempty"`
	Signal     string `json:"signal,omitempty"`
	Scope      string `json:"scope,omitempty"`
	WaitForLog string `json:"wait_for_log,omitempty"`
	Wait       Millis `json:"wait_ms,omitempty"`
	OnTimeout  string `json:"on_timeout,omitempty"`

	// TolerateDisconnect makes a hangup on this step a success rather than a
	// failure. See disconnected.
	TolerateDisconnect bool `json:"tolerate_disconnect,omitempty"`
}

// OnTimeoutEscalate is the manifest value that abandons the remaining declared
// steps and goes straight to the implicit signal rungs.
const OnTimeoutEscalate = "escalate"

// Plan is the rendered ladder plus everything the standalone path needs to
// execute it without gvm's configuration.
type Plan struct {
	Proto    int      `json:"proto"`
	Instance string   `json:"instance"`
	Game     string   `json:"game,omitempty"`
	Unit     string   `json:"unit,omitempty"`
	Process  Identity `json:"process"`

	// ConsoleFD is where the standalone path writes a `via: console` step:
	// /proc/<pid>/fd/0, the game's own stdin. It is a degraded channel -- there
	// is no ring, so no echo and no wait_for_log -- and section 11.8 says so
	// plainly: honest, and better than nothing.
	ConsoleFD string `json:"console_fd,omitempty"`
	// LineEnding is the real bytes, not the manifest's name for them, because
	// the standalone path does not load the manifest.
	LineEnding string `json:"line_ending,omitempty"`

	Budget                 Millis `json:"budget_ms,omitempty"`
	ExtendWhileProgressing bool   `json:"extend_while_progressing,omitempty"`
	ProgressWindow         Millis `json:"progress_window_ms,omitempty"`

	Steps []PlanStep `json:"steps"`

	ShutdownLog  string `json:"shutdown_log,omitempty"`
	LastShutdown string `json:"last_shutdown,omitempty"`
}

// BuildPlan renders a manifest's shutdown ladder for one instance.
//
// Every refusal here happens at spawn, minutes or months before anyone needs
// the world stopped. That is the entire reason this is a separate function
// from the ladder that runs it.
func BuildPlan(m *spec.Manifest, r *spec.Renderer) ([]PlanStep, error) {
	if m == nil {
		return nil, errors.New("runner: BuildPlan needs a manifest")
	}
	stopControl := ""
	if v, ok := m.Spec.Verbs["stop"]; ok {
		stopControl = strings.TrimSpace(v.Control)
	}

	steps := make([]PlanStep, 0, len(m.Spec.Shutdown.Steps))
	for i, st := range m.Spec.Shutdown.Steps {
		field := fmt.Sprintf("spec.shutdown.steps[%d]", i)
		if _, err := RungFor(st.Via); err != nil {
			return nil, fmt.Errorf("%s.via: %w", field, err)
		}
		ps := PlanStep{
			Via:        st.Via,
			WaitForLog: st.WaitForLog,
			Wait:       MillisOf(st.Wait.Duration()),
			OnTimeout:  st.OnTimeout,
		}
		if st.Send != "" {
			send, taint, err := renderStep(r, field+".send", st.Send)
			if err != nil {
				return nil, err
			}
			// A shutdown command that interpolated a secret could not be
			// written to shutdown.json, which is 0640 and readable by the
			// console group -- so the standalone path would have nothing to
			// send. Refusing is the honest answer: no shipped manifest needs
			// it, and silently redacting would produce a ladder that sends
			// the literal string "••••••" to a game console.
			if taint.Has(spec.TaintSecret) {
				return nil, fmt.Errorf("%s.send interpolates a secret; a shutdown step must not, "+
					"because the rendered ladder is written to shutdown.json for `gvm run-shutdown`", field)
			}
			if err := console.ValidateSendLine(send); err != nil {
				return nil, fmt.Errorf("%s.send: %w", field, err)
			}
			ps.Send = send
		}
		switch st.Via {
		case spec.ViaSignal:
			sig, err := platform.ParseSignal(st.Signal)
			if err != nil {
				return nil, fmt.Errorf("%s.signal: %w", field, err)
			}
			ps.Signal = sig.String()
			sc, err := ParseScope(st.Scope)
			if err != nil {
				return nil, fmt.Errorf("%s.scope: %w", field, err)
			}
			ps.Scope = sc.String()
		case spec.ViaConsole, spec.ViaControl:
			if ps.Send == "" {
				return nil, fmt.Errorf("%s: a %s step needs a `send`", field, st.Via)
			}
		}
		if st.WaitForLog != "" {
			// Compiled here and thrown away, so a bad pattern is a refusal at
			// spawn rather than a rung that silently degrades to a blind sleep
			// at the moment it matters.
			if _, err := probe.NewLogMarker(console.NewRing(1<<10, 1), st.WaitForLog, 1); err != nil {
				return nil, fmt.Errorf("%s.wait_for_log: %w", field, err)
			}
		}
		if st.OnTimeout != "" && st.OnTimeout != "continue" && st.OnTimeout != OnTimeoutEscalate {
			return nil, fmt.Errorf("%s.on_timeout: %q is not continue|escalate", field, st.OnTimeout)
		}

		ps.TolerateDisconnect = tolerateDisconnect(st, stopControl)
		steps = append(steps, ps)
	}
	return steps, nil
}

// tolerateDisconnect resolves the manifest's tri-state.
//
// The default is TRUE for a control step that sends the game's own stop
// command, because that command's correct outcome is the socket closing under
// it. `DialectSimple` reads until idle; a `quit` that works closes the
// connection first, so a strict reading reports failure on a shutdown that
// went perfectly -- and a ladder that believes its stop step failed escalates
// to SIGTERM against a server that is already saving.
func tolerateDisconnect(st spec.ShutdownStep, stopControl string) bool {
	if st.TolerateDisconnect != nil {
		return *st.TolerateDisconnect
	}
	return st.Via == spec.ViaControl && stopControl != "" && strings.TrimSpace(st.Send) == stopControl
}

// renderStep expands one shutdown template. A nil renderer is accepted only
// for a template with nothing in it to expand, matching the Commander's rule:
// rendering against a zero RenderData would turn a command into a shorter,
// wrong command and report success.
func renderStep(r *spec.Renderer, field, tmpl string) (string, spec.Taint, error) {
	if r == nil {
		if strings.Contains(tmpl, "{{") {
			return "", spec.TaintNone, fmt.Errorf("runner: %s needs a renderer: %q", field, tmpl)
		}
		return tmpl, spec.TaintNone, nil
	}
	return r.Render(field, tmpl)
}

// WritePlan writes shutdown.json atomically.
func WritePlan(p string, pl Plan) error {
	pl.Proto = PlanProto
	b, err := json.MarshalIndent(pl, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, append(b, '\n'), 0o640)
}

// ReadPlan decodes shutdown.json.
func ReadPlan(p string) (Plan, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return Plan{}, err
	}
	var pl Plan
	if err := json.Unmarshal(b, &pl); err != nil {
		return Plan{}, fmt.Errorf("runner: %s: %w", p, err)
	}
	if pl.Proto != PlanProto {
		return Plan{}, fmt.Errorf("runner: %s is proto %d, this build speaks %d", p, pl.Proto, PlanProto)
	}
	return pl, nil
}

// ConsoleSender injects one line towards the game and returns the Seq of the
// echo the runner appended to the ring.
//
// It is a local interface rather than control.Sender so the ladder compiles
// against the runner alone; control.Sender and *Runner both satisfy it.
type ConsoleSender interface {
	Send(actor, line string) (seq uint64, err error)
}

// ControlChannel is the control channel as the ladder needs it, structurally
// satisfied by control.Channel.
type ControlChannel interface {
	Ready() bool
	Unready() string
	Exec(ctx context.Context, cmd string) (string, error)
}

// Ladder executes a Plan against a Process and produces a Report.
//
// Everything it touches is an interface or a plain value, so the whole thing
// -- including the rungs that end in kill(2) -- runs against a fake process on
// the Windows development machine. That is not a convenience. This is the code
// that decides whether a world is saved, and code that can only be exercised
// on the production box is code that gets exercised on the production box.
type Ladder struct {
	Plan Plan
	Proc Process

	// Ring is the console history. nil on the standalone path, where
	// wait_for_log has nothing to watch and degrades to a plain wait.
	Ring *console.Ring
	// Console carries `via: console` steps. nil when the game captures none.
	Console ConsoleSender
	// Control carries `via: control` steps. nil when the game has none, which
	// is half the catalogue.
	Control ControlChannel

	Clock  console.Clock
	Notify *platform.Notifier
	Log    logger
	// Note annotates the console. A shutdown that says nothing in the log the
	// operator is watching is a forty-second silence they cannot interpret.
	Note func(format string, a ...any) uint64
	// Step narrates each rung to an attached client, which is the difference
	// between "stopping..." and "sending save, waiting for the marker" during
	// a ninety second save.
	Step func(rung, send string)

	// Progress reports when the game last produced a byte, for rule 5's
	// EXTEND_TIMEOUT_USEC. nil disables the extension.
	Progress func() time.Time
	// Extended accumulates the microseconds asked for, shared with readiness
	// so one report can answer "why did systemd wait eleven minutes".
	Extended *atomic.Int64

	// exit is what the watcher reaped, for a caller that wants the real status
	// rather than the report's rendering of it. Valid only when exited.
	exit   ExitStatus
	exited bool
	// abandoned is set when a rung's wait ended because the CALLER went away.
	// It is sticky and it stops the ladder: see finishWait.
	abandoned bool
}

// Exit reports the child's status as this ladder observed it.
func (l *Ladder) Exit() (ExitStatus, bool) { return l.exit, l.exited }

// logger is the sliver of *slog.Logger the ladder uses.
type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

func (l *Ladder) clock() console.Clock {
	if l.Clock == nil {
		return console.SystemClock{}
	}
	return l.Clock
}

func (l *Ladder) note(format string, a ...any) {
	if l.Log != nil {
		l.Log.Info("shutdown: " + fmt.Sprintf(format, a...))
	}
	if l.Note != nil {
		l.Note("-- gvm: "+format, a...)
	}
}

func (l *Ladder) announce(rung Rung, send string) {
	if l.Step != nil {
		l.Step(string(rung), send)
	}
}

func (l *Ladder) extend(usec int64) {
	if l.Extended != nil {
		l.Extended.Add(usec)
	}
}

// Run executes the ladder and BLOCKS until the game's process group is empty
// or the ladder runs out of rungs.
//
// It never returns an error. A shutdown that went wrong is a Report that says
// so: an error would give the caller the option of dropping the record, and
// the record is the product.
func (l *Ladder) Run(ctx context.Context, actor, reason string) *Report {
	clk := l.clock()
	start := clk.Now()

	rep := &Report{
		Proto:      ReportProto,
		Instance:   l.Plan.Instance,
		Game:       l.Plan.Game,
		Unit:       l.Plan.Unit,
		Reason:     reason,
		Actor:      actor,
		StartedAt:  start,
		ExitCode:   -1,
		Standalone: l.Ring == nil,
	}

	// STOPPING=1 first, so systemd stops counting the unit as running while
	// the ladder does its slow, correct work.
	if l.Notify != nil {
		_ = l.Notify.Stopping()
		_ = l.Notify.Status(fmt.Sprintf("stopping: %s (%s)", reason, actor))
	}
	l.note("shutdown requested by %s: %s", actor, reason)

	// One cancellable context for the watchers, ended before the report is
	// written so no goroutine is still touching the process afterwards.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	w := watchExit(wctx, l.Proc, l.recoverInto)
	if l.Plan.ExtendWhileProgressing && l.Plan.ProgressWindow > 0 && l.Progress != nil {
		go l.extendLoop(wctx)
	}

	l.runRungs(ctx, w, rep, actor)
	if l.abandoned {
		rep.Degraded = true
		rep.Err = "the shutdown was abandoned before it finished; the game may still be running"
		l.note("the caller went away mid-shutdown; the remaining rungs were NOT run and the game was not killed")
	}

	rep.FinishedAt = clk.Now()
	rep.Duration = MillisOf(rep.FinishedAt.Sub(rep.StartedAt))

	// A ladder can finish knowing the process is gone before the watcher has
	// delivered its status -- most obviously when the game had already exited
	// and no rung ever ran. Give the handoff a moment, or the report says
	// "exit_code: -1" for a server that exited cleanly.
	if settled, _ := l.settled(); settled {
		w.awaitReap(reapGrace)
	}
	if st, ok := w.status(); ok {
		l.exit, l.exited = st, true
		rep.Exited, rep.ExitCode, rep.Signal = true, st.Code, st.SignalName()
	} else if settled, _ := l.settled(); settled {
		// The game is gone but this ladder never reaped it -- the standalone
		// path, where nobody is the parent. -1 rather than an invented 0: only
		// a parent learns an exit status, and pretending otherwise would make
		// "stopped cleanly" a value gvm made up.
		rep.Exited = true
	}
	if l.Extended != nil {
		rep.ExtendedUsec = l.Extended.Load()
	}
	rep.ConsoleTail = tailFrom(l.Ring)

	l.note("shutdown finished at rung %s after %s (degraded=%t)",
		rep.FinalRung, rep.Duration, rep.Degraded)
	return rep
}

// runRungs walks the declared steps and then the implicit ones, filling rep.
func (l *Ladder) runRungs(ctx context.Context, w *exitWatcher, rep *Report, actor string) {
	// Nothing to do is a real answer: a game that crashed on its own two
	// seconds before the operator pressed stop is not a degraded shutdown.
	if settled, why := l.settled(); settled {
		rep.FinalRung = RungNone
		l.note("the game had already exited; no rung was needed")
		l.noteAdvisory(why)
		return
	}

	deadline, hasDeadline := l.deadline(rep.StartedAt)

	for i, ps := range l.Plan.Steps {
		if settled, why := l.settled(); settled {
			l.noteAdvisory(why)
			return
		}
		if hasDeadline && !l.clock().Now().Before(deadline) {
			// Out of declared budget. Recording the skip matters: without it
			// the report would show a ladder that simply never ran `save`,
			// with no hint that TimeoutStopSec was the reason.
			rep.Steps = append(rep.Steps, StepResult{
				Index: i, Rung: rungOrEmpty(ps.Via), Via: ps.Via, Send: ps.Send,
				StartedAt: l.clock().Now(), Outcome: OutcomeSkipped, Alive: true,
				Note: fmt.Sprintf("the %s shutdown budget was spent before this rung", l.Plan.Budget),
			})
			continue
		}

		res := l.runStep(ctx, w, i, ps, actor, deadline, hasDeadline)
		rep.Steps = append(rep.Steps, res)
		rep.FinalRung = res.Rung
		if degradesBySignal(ps) {
			// Set BEFORE the !res.Alive return below, which is the branch that
			// otherwise skips all degradation bookkeeping. A manifest can name
			// SIGKILL, and a world the ladder ended rather than stopped is
			// degraded whoever asked for it.
			rep.Degraded = true
		}
		if l.abandoned {
			return
		}
		if !res.Alive {
			return
		}
		if res.Outcome == OutcomeTimeout && ps.OnTimeout == OnTimeoutEscalate {
			l.note("step %d timed out with on_timeout: escalate; skipping the remaining declared steps", i+1)
			break
		}
	}

	l.runImplicit(ctx, w, rep, deadline, hasDeadline)
}

// runImplicit is section 11.6 rule 7: after the last declared step, SIGTERM to
// the process group, then SIGKILL to the process group, then SIGKILL to every
// pid still in the unit's cgroup. Either SIGKILL is a degraded shutdown.
func (l *Ladder) runImplicit(ctx context.Context, w *exitWatcher, rep *Report, deadline time.Time, hasDeadline bool) {
	for _, rung := range []struct {
		rung  Rung
		sig   platform.Signal
		scope Scope
		wait  time.Duration
	}{
		{RungSIGTERM, platform.SIGTERM, ScopeGroup, implicitTermWait},
		{RungSIGKILL, platform.SIGKILL, ScopeGroup, implicitKillWait},
		{RungCgroup, platform.SIGKILL, ScopeCgroup, implicitKillWait},
	} {
		// settled is authoritative and `why` is advisory. The two cases it
		// folds together are exactly the two the sweep must not confuse: a
		// dead leader with a provably empty cgroup, and a dead leader whose
		// cgroup gvm is not entitled to read. Both stop the ladder; only the
		// first is silent about it.
		if settled, why := l.settled(); settled {
			l.noteAdvisory(why)
			return
		}

		l.announce(rung.rung, rung.sig.String())
		started := l.clock().Now()
		// Clamped like a declared rung: the signal is always DELIVERED, but
		// waiting past TimeoutStopSec for it to take effect only means systemd
		// kills the runner before it can say what it did.
		wait := rung.wait
		if hasDeadline {
			if left := deadline.Sub(started); left < wait {
				wait = left
			}
		}
		res := l.signalRung(ctx, w, StepResult{
			Index: -1, Rung: rung.rung,
			Signal: rung.sig.String(), Scope: rung.scope.String(),
			Budget: MillisOf(wait), StartedAt: started, Alive: true,
		}, rung.sig, rung.scope, wait)

		rep.Steps = append(rep.Steps, res)
		rep.FinalRung = rung.rung
		if l.abandoned {
			return
		}
		if rung.rung.Degrades() {
			// Reaching a SIGKILL rung is degraded whether or not it worked:
			// the world was not asked to save, it was ended. These rungs have
			// names, so the rule reads off the name here and off the SIGNAL
			// for a declared step, which has none -- and
			// TestTheTwoDegradationRulesAgree pins the two together.
			rep.Degraded = true
		}
		if !res.Alive {
			return
		}
	}
	// Out of rungs with something still running. The runner is about to exit
	// and systemd's KillMode=mixed will reap the cgroup; the report says the
	// ladder did not finish the job.
	rep.Degraded = true
	l.note("the game is still running after every rung; systemd will reap the cgroup")
}

// degradesBySignal reports whether a declared step ends the world rather than
// stopping it. It is the same rule runImplicit applies, in one place, so the
// declared and implicit ladders cannot disagree about what `degraded` means --
// and `degraded` is the field gvmd indexes and alerts on.
func degradesBySignal(ps PlanStep) bool {
	if ps.Via != spec.ViaSignal {
		return false
	}
	sig, err := platform.ParseSignal(ps.Signal)
	return err == nil && sig == platform.SIGKILL
}

// runStep executes one declared rung.
func (l *Ladder) runStep(ctx context.Context, w *exitWatcher, i int, ps PlanStep, actor string,
	deadline time.Time, hasDeadline bool) StepResult {

	clk := l.clock()
	res := StepResult{
		Index: i, Rung: rungOrEmpty(ps.Via), Via: ps.Via,
		Send: ps.Send, Signal: ps.Signal, Scope: ps.Scope,
		WaitForLog: ps.WaitForLog, Budget: ps.Wait,
		StartedAt: clk.Now(), Alive: true,
	}
	budget := ps.Wait.Duration()
	if hasDeadline {
		if left := deadline.Sub(res.StartedAt); left < budget {
			budget = left
			res.Note = appendNote(res.Note, fmt.Sprintf(
				"the wait was cut to %s by the %s shutdown budget", budget.Truncate(time.Millisecond), l.Plan.Budget))
		}
	}
	// The record carries the budget this rung ACTUALLY had, not the one the
	// manifest asked for: a step that shows `budget_ms: 300000` and a 40 s
	// duration would look like an early success rather than a clamp.
	res.Budget = MillisOf(budget)

	l.announce(res.Rung, firstNonEmpty(ps.Send, ps.Signal))

	switch ps.Via {
	case spec.ViaSignal:
		sig, err := platform.ParseSignal(ps.Signal)
		if err != nil {
			return l.finishStep(res, OutcomeFailed, err)
		}
		scope, err := ParseScope(ps.Scope)
		if err != nil {
			return l.finishStep(res, OutcomeFailed, err)
		}
		return l.signalRung(ctx, w, res, sig, scope, budget)

	case spec.ViaWaitExit:
		l.note("step %d/%d: waiting up to %s for the game to exit",
			i+1, len(l.Plan.Steps), budget.Truncate(time.Second))
		return l.finishWait(ctx, res, w, nil, budget)

	case spec.ViaConsole:
		return l.consoleStep(ctx, w, res, i, ps, actor, budget)

	case spec.ViaControl:
		return l.controlStep(ctx, w, res, i, ps, budget)
	}
	return l.finishStep(res, OutcomeFailed, fmt.Errorf("runner: %q is not a shutdown channel", ps.Via))
}

// consoleStep sends a line to the game's stdin and waits for its marker.
//
// The ORDER in this function is section 11.6 rule 1 and one addition to it.
// Rule 1: capture the ring cursor BEFORE the send, so a `save` typed an hour
// ago cannot satisfy the wait that guards `quit`. The addition: step past the
// echo of our own command, because Send appends the command itself to the ring
// as SourceActor, and a marker pattern loose enough to match `save` would
// otherwise be satisfied instantly by gvm's own echo -- the same stale-match
// bug, one line lower down. The console annotation is written first for the
// same reason.
func (l *Ladder) consoleStep(ctx context.Context, w *exitWatcher, res StepResult,
	i int, ps PlanStep, actor string, budget time.Duration) StepResult {

	if l.Console == nil {
		res.Note = "this game captures no console, so the step could not be attempted"
		return l.finishStep(res, OutcomeSkipped, nil)
	}
	l.note("step %d/%d: console send %q", i+1, len(l.Plan.Steps), ps.Send)

	since := uint64(0)
	if l.Ring != nil {
		since = probe.After(l.Ring)
	}
	seq, err := l.Console.Send(actor, ps.Send)
	if err != nil {
		// The line did not reach the game. Recorded and the ladder continues:
		// 7 Days to Die's ladder deliberately follows every control rung with
		// the same command over the console, and a delivery failure that
		// escalated straight to SIGTERM would defeat exactly that pattern.
		return l.finishStep(res, OutcomeFailed, err)
	}
	res.EchoSeq = seq
	if seq >= since {
		since = seq + 1
	}
	res.SinceSeq = since

	marker := l.marker(&res, ps.WaitForLog, since)
	return l.finishWait(ctx, res, w, marker, budget)
}

// controlStep runs one command over the control channel.
func (l *Ladder) controlStep(ctx context.Context, w *exitWatcher, res StepResult,
	i int, ps PlanStep, budget time.Duration) StepResult {

	switch {
	case l.Control == nil:
		res.Note = "this game has no control channel, so the step could not be attempted"
		return l.finishStep(res, OutcomeSkipped, nil)
	case !l.Control.Ready():
		// Not a failure. Project Zomboid's RCON is unavailable for the whole
		// first boot and 7DTD's telnet is not implemented in this build; in
		// both cases the console rung that follows is the one that works.
		res.Note = firstNonEmpty(l.Control.Unready(), "the control channel is not ready")
		return l.finishStep(res, OutcomeSkipped, nil)
	}
	l.note("step %d/%d: control send %q", i+1, len(l.Plan.Steps), ps.Send)

	since := uint64(0)
	if l.Ring != nil {
		since = probe.After(l.Ring)
	}
	res.SinceSeq = since

	out, err := l.Control.Exec(ctx, ps.Send)
	res.Output = out
	if err != nil {
		if ps.TolerateDisconnect && disconnected(err) {
			// The server hung up, which for a command whose whole purpose is
			// to make it exit is the expected reply, not a failure.
			res.Note = appendNote(res.Note, "the server closed the connection, which is the expected reply to a stop command")
			res.Err = err.Error()
		} else {
			return l.finishStep(res, OutcomeFailed, err)
		}
	}
	marker := l.marker(&res, ps.WaitForLog, since)
	return l.finishWait(ctx, res, w, marker, budget)
}

// marker builds the wait_for_log probe, or explains why there is none.
func (l *Ladder) marker(res *StepResult, pattern string, since uint64) *probe.LogMarker {
	if pattern == "" {
		return nil
	}
	if l.Ring == nil {
		// The standalone path. Say so in the record rather than letting the
		// step look like it waited for a marker and timed out.
		res.Note = appendNote(res.Note, "no console ring on this path, so the marker could not be watched")
		return nil
	}
	m, err := probe.NewLogMarker(l.Ring, pattern, since)
	if err != nil {
		res.Err = err.Error()
		res.Note = appendNote(res.Note, "the marker could not be compiled; this rung degraded to a plain wait")
		return nil
	}
	return m
}

// signalRung delivers one signal and waits for the process to go.
func (l *Ladder) signalRung(ctx context.Context, w *exitWatcher, res StepResult,
	sig platform.Signal, scope Scope, budget time.Duration) StepResult {

	l.note("%s to the %s (%s)", sig, scope, l.Proc.Identity())

	err := l.Proc.Signal(sig, scope)
	switch {
	case errors.Is(err, ErrGone):
		// Nothing left to signal: the rung achieved what it was asked to.
		res.Note = appendNote(res.Note, "there was nothing left to signal")
		return l.finishStep(res, OutcomeExited, nil)
	case err != nil:
		// A failed delivery is a rung that did NOT work, and the ladder must
		// escalate rather than wait out a budget for an exit that is not
		// coming.
		return l.finishStep(res, OutcomeFailed, err)
	}
	return l.finishWait(ctx, res, w, nil, budget)
}

// finishWait blocks for budget, ending early on an exit or a marker match.
func (l *Ladder) finishWait(ctx context.Context, res StepResult, w *exitWatcher,
	marker *probe.LogMarker, budget time.Duration) StepResult {

	if budget <= 0 {
		if marker != nil {
			res.Note = appendNote(res.Note, "this step has no `wait`, so its marker was not awaited")
		}
		// Still ask whether the game is gone: a `quit` that took effect
		// immediately must end the ladder here rather than at the next rung.
		return l.finishStep(res, OutcomeOK, nil)
	}

	timeout := make(chan struct{})
	t := l.clock().AfterFunc(budget, func() { close(timeout) })
	defer t.Stop()

	var matched chan console.Line
	if marker != nil {
		matched = make(chan console.Line, 1)
		mctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			defer l.recoverInto("shutdown wait_for_log")
			line, err := marker.Wait(mctx)
			if err == nil {
				matched <- line
			}
		}()
	}

	select {
	case <-w.gone:
		return l.finishStep(res, OutcomeExited, nil)
	case line := <-matched:
		res.MatchedSeq, res.Matched = line.Seq, line.Text
		return l.finishStep(res, OutcomeOK, nil)
	case <-timeout:
		if marker != nil {
			// The hurry-up expired. `on_timeout: continue` is the default and
			// the bash suite's blind `sleep 10` is the worst case this
			// reproduces, so advancing is correct -- but it is recorded, which
			// the bash suite could not do.
			res.Note = appendNote(res.Note, "the marker did not appear within the wait")
		}
		return l.finishStep(res, OutcomeTimeout, nil)
	case <-ctx.Done():
		// The caller went away mid-rung. Every remaining rung would still
		// DELIVER its side effect -- Console.Send and Proc.Signal take no
		// context -- and every remaining wait would return instantly on the
		// closed Done channel, so Project Zomboid's five and a half minute
		// ladder would collapse into microseconds and SIGKILL a JVM in the
		// middle of a world write. Stopping is the safe default: a world that
		// is still running can be stopped again, a killed one cannot be
		// un-killed.
		l.abandoned = true
		return l.finishStep(res, OutcomeFailed, ctx.Err())
	}
}

// finishStep stamps the duration, re-reads liveness and returns the result.
func (l *Ladder) finishStep(res StepResult, out Outcome, err error) StepResult {
	res.Outcome = out
	if err != nil {
		res.Err = err.Error()
	}
	res.Duration = MillisOf(l.clock().Now().Sub(res.StartedAt))

	settled, why := l.settled()
	res.Alive = !settled
	if settled && res.Outcome == OutcomeTimeout {
		// The wait expired at the same moment the game went. Reporting a
		// timeout on a rung that in fact ended the process would put the wrong
		// final rung in the record.
		res.Outcome = OutcomeExited
	}
	if why != nil {
		// Advisory, so it goes in the note and not in Err: "gvm could not see
		// into the cgroup" is a limit on what this rung PROVED, not a claim
		// that the rung failed.
		res.Note = appendNote(res.Note, why.Error())
	}
	return res
}

// noteAdvisory annotates the console when the ladder stopped without being
// able to see the whole picture. It is a note rather than a failure because
// the alternative -- treating "cannot see" as "still running" -- is a ladder
// that loops forever on a sweep it is not allowed to perform.
func (l *Ladder) noteAdvisory(why error) {
	if why != nil {
		l.note("%v", why)
	}
}

// settled reports whether there is anything left for a rung to act on.
//
// Two questions, in order, and the second one only when the first says the
// leader is gone: is the pinned process still alive, and is the unit's cgroup
// empty. The pair matters because the failure this whole subsystem exists for
// is a launcher script that exits while the engine it forked keeps writing to
// the world.
//
// An error means "cannot tell", and cannot tell is never "the world is free".
// The one exception is ErrNoCgroup with a dead leader: the answer is settled as
// far as gvm can prove, because the alternative is a ladder that loops forever
// on a sweep it is not allowed to perform.
func (l *Ladder) settled() (bool, error) {
	alive, err := l.Proc.Alive()
	if err != nil {
		return false, fmt.Errorf("gvm could not read the game's liveness, so it is treated as still running: %w", err)
	}
	if alive {
		return false, nil
	}
	empty, err := l.Proc.Empty()
	if err != nil {
		return true, fmt.Errorf("the pinned process is gone and gvm cannot see into the cgroup to check for survivors: %w", err)
	}
	return empty, nil
}

// deadline is when every rung -- declared and implicit -- must stop waiting,
// keeping reportReserve back so the record still gets written.
func (l *Ladder) deadline(start time.Time) (time.Time, bool) {
	budget := l.Plan.Budget.Duration()
	if budget <= 0 {
		// No budget declared means no self-imposed wall. systemd's
		// TimeoutStopSec is then the only bound, which is the pre-gvm
		// behaviour and is honest about it.
		return time.Time{}, false
	}
	if budget <= reportReserve {
		return start.Add(budget), true
	}
	return start.Add(budget - reportReserve), true
}

// extendLoop is section 11.6 rule 5: keep asking systemd for more time while
// the game is still producing output, so a genuinely slow save is not
// guillotined at TimeoutStopSec while a hung one still dies on schedule.
func (l *Ladder) extendLoop(ctx context.Context) {
	defer l.recoverInto("shutdown extend loop")

	window := l.Plan.ProgressWindow.Duration()
	every := window / 3
	if every <= 0 {
		return
	}
	clk := l.clock()
	for {
		if err := sleepOn(ctx, clk, every); err != nil {
			return
		}
		if clk.Now().Sub(l.Progress()) > window {
			// Silent. Stop extending and let TimeoutStopSec do its job.
			continue
		}
		if l.Notify != nil {
			_ = l.Notify.ExtendTimeout(window)
		}
		l.extend(window.Microseconds())
	}
}

// recoverInto is section 11.8 layer 1 for this package's shutdown goroutines.
func (l *Ladder) recoverInto(what string) {
	rec := recover()
	if rec == nil {
		return
	}
	defer func() { _ = recover() }()
	msg := fmt.Sprintf("-- gvm: %s panicked: %v", what, rec)
	if l.Log != nil {
		l.Log.Error(msg)
	}
	if l.Ring != nil {
		l.Ring.Append(console.Line{Source: console.SourceRunner, Text: msg})
	}
}

// exitWatcher turns Process.Wait into a channel every rung can select on.
//
// One watcher for the whole ladder rather than one per rung, because `gone` is
// a closed channel: a rung that starts after the game has already exited sees
// it immediately instead of waiting out its own budget.
type exitWatcher struct {
	gone chan struct{}
	once sync.Once

	mu sync.Mutex
	st ExitStatus
	ok bool
}

func watchExit(ctx context.Context, p Process, recoverInto func(string)) *exitWatcher {
	w := &exitWatcher{gone: make(chan struct{})}
	go func() {
		defer recoverInto("shutdown exit watcher")
		st, err := p.Wait(ctx)
		if err != nil {
			// A cancelled context or an unreadable /proc. NOT an exit: closing
			// `gone` here would tell every remaining rung the game is dead and
			// end the ladder on a live world.
			return
		}
		w.mu.Lock()
		w.st, w.ok = st, true
		w.mu.Unlock()
		w.once.Do(func() { close(w.gone) })
	}()
	return w
}

func (w *exitWatcher) status() (ExitStatus, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.st, w.ok
}

// reapGrace bounds the wait for the exit watcher's handoff.
//
// It is the one timer in the ladder that deliberately does NOT come from the
// injected clock, for the same reason Runner.drainPump's does not: it is a
// watchdog, and a watchdog wired to a clock the caller controls cannot bark.
// It only ever runs when the process is already known to be gone, so in the
// normal case it is stopped microseconds after it is armed.
const reapGrace = time.Second

func (w *exitWatcher) awaitReap(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-w.gone:
	case <-t.C:
	}
}

// disconnected reports whether err is the server hanging up.
//
// Section 11.6 rule 3's list, plus the two errno spellings a closed game
// socket produces. It is a closed set on purpose: "any error on a step that
// tolerates disconnects" would swallow an authentication failure and report a
// shutdown that never reached the server.
var disconnectSentinels = []error{
	io.EOF,
	io.ErrUnexpectedEOF,
	net.ErrClosed,
	syscall.ECONNRESET,
	syscall.EPIPE,
	rcon.ErrClosed,
	control.ErrClosed,
}

func disconnected(err error) bool {
	if err == nil {
		return false
	}
	for _, s := range disconnectSentinels {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

// sleepOn waits d on the injected clock, or until ctx ends.
func sleepOn(ctx context.Context, c console.Clock, d time.Duration) error {
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

// rungOrEmpty is RungFor for a value the plan already validated.
func rungOrEmpty(via string) Rung {
	r, err := RungFor(via)
	if err != nil {
		return ""
	}
	return r
}

func appendNote(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ErrStopInProgress refuses a second concurrent ladder.
//
// One ladder at a time is a property the runner owns, not the socket: the
// socket refuses a second `stop` op, but a hook, a signal handler and an
// attached client can all arrive by different routes, and two ladders on one
// world would race a `save` against a SIGTERM.
var ErrStopInProgress = errors.New("runner: a shutdown is already in progress")

// SetControl attaches the control channel the ladder's `via: control` steps
// use. It is a setter rather than a Config field because the channel's gate
// closes over the runner's own state, so the runner has to exist first.
func (r *Runner) SetControl(ch control.Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.control = ch
}

// Control returns the attached control channel, nil when there is none.
func (r *Runner) Control() control.Channel {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.control
}

// StopRequested is closed once a shutdown ladder has started, and stays
// closed. A readiness wait selects on it: a stop is a legal transition out of
// `booting`, and nothing else on that path could ever learn about one.
func (r *Runner) StopRequested() <-chan struct{} { return r.stopChan() }

func (r *Runner) stopChan() chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopCh == nil {
		r.stopCh = make(chan struct{})
	}
	return r.stopCh
}

// markStopRequested closes the channel StopRequested hands out, exactly once.
func (r *Runner) markStopRequested() {
	ch := r.stopChan()
	r.stopOnce.Do(func() { close(ch) })
}

// Stopping reports whether a shutdown ladder is running.
//
// The flag and the state are both consulted because Shutdown sets them one
// after the other -- it wins the CAS first and moves to `stopping` second --
// and a caller that arrived in between would otherwise see a runner that is
// about to start saving as an idle one.
func (r *Runner) Stopping() bool {
	return r.stopping.Load() || r.State() == StateStopping
}

// ControlGateOpen reports whether the game's control port is worth dialling.
// It is rule 1 of section 11.9 -- never dial before the game is listening --
// and `gvm run` hands this method straight to the RCON client as its gate.
//
// `stopping` is included, and it has to be: Shutdown moves the runner to
// `stopping` BEFORE it builds the ladder, so a gate that closed at `ready`
// would skip every `via: control` rung on the way down. Palworld's ladder is
// `Save` then `DoExit` over RCON with no console equivalent, so that gate cost
// the world its save on every stop and reported `degraded: false` afterwards.
// The socket is still bound and still answering while the game saves.
//
// The ReadyAt guard is what keeps that honest. `booting -> stopping` is a
// legal transition, and a stop that arrives during Project Zomboid's forty
// minute boot never had a port to talk to; opening the gate for it would turn
// an honest skip carrying the manifest's not_ready_reason into a failure
// carrying a connection-refused string, which is the log noise the gate exists
// to prevent.
func (r *Runner) ControlGateOpen() bool {
	switch r.State() {
	case StateReady:
		return true
	case StateStopping:
		return !r.ReadyAt().IsZero()
	}
	return false
}

// ladderControl is r.control as the ladder wants it, and nil when there is
// none. A typed nil interface would make every control rung attempt a call
// against a nil pointer instead of being honestly recorded as skipped.
func (r *Runner) ladderControl() ControlChannel {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.control == nil {
		return nil
	}
	return r.control
}

// Plan is the rendered ladder for this instance, with the pinned identity when
// the game has been spawned. It is what `gvm run-shutdown` reads.
func (r *Runner) Plan() Plan {
	r.mu.Lock()
	id := r.identity
	r.mu.Unlock()

	sd := r.cfg.Manifest.Spec.Shutdown
	pl := Plan{
		Proto:                  PlanProto,
		Instance:               r.cfg.Instance,
		Game:                   r.cfg.Game,
		Unit:                   r.cfg.Unit,
		Process:                id,
		LineEnding:             r.launch.Console.LineEnding,
		Budget:                 MillisOf(sd.Budget.Duration()),
		ExtendWhileProgressing: sd.ExtendWhileProgressing,
		ProgressWindow:         MillisOf(sd.ProgressWindow.Duration()),
		Steps:                  r.steps,
		ShutdownLog:            r.cfg.Paths.ShutdownLog,
		LastShutdown:           r.cfg.Paths.LastShutdown,
	}
	if id.PID > 0 && r.launch.Console.Capturing() {
		// The game's own stdin, which is the only console the standalone path
		// has. It is written as a path rather than resolved here because
		// nothing may hold that descriptor open for the life of the runner:
		// an open write end of a game's stdin is a supervisor that cannot tell
		// the game it is done talking.
		pl.ConsoleFD = fmt.Sprintf("/proc/%d/fd/0", id.PID)
	}
	return pl
}

// writePlanFile publishes the ladder for `gvm run-shutdown`.
func (r *Runner) writePlanFile() error {
	if r.cfg.Paths.ShutdownJSON == "" {
		return nil
	}
	return WritePlan(r.cfg.Paths.ShutdownJSON, r.Plan())
}

// Shutdown runs the manifest ladder and BLOCKS until the game's process group
// is empty. It is the only function in gvm permitted to signal the game.
//
// It always returns a Report, including for the refusals: a caller that asked
// for a shutdown is owed a record of what happened, and "another one was
// already running" is what happened.
func (r *Runner) Shutdown(ctx context.Context, actor, reason string) *Report {
	if !r.stopping.CompareAndSwap(false, true) {
		return r.refusedReport(actor, reason, ErrStopInProgress)
	}
	// Announced before anything else, so a readiness wait blocked on a
	// forty-minute boot learns immediately that the world is being stopped
	// rather than idling to stall_timeout on it.
	r.markStopRequested()

	// The barrier a teardown waits on. It is closed only after rep.Save has
	// returned, so a process that exits the moment AwaitStop unblocks still
	// leaves the record behind -- and the record is the product.
	r.mu.Lock()
	done := make(chan struct{})
	r.stopDone, r.stopReport = done, nil
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.stopDone = nil
		r.mu.Unlock()
		r.stopping.Store(false)
		close(done)
	}()

	r.mu.Lock()
	proc := r.proc
	r.mu.Unlock()
	if proc == nil {
		return r.refusedReport(actor, reason, ErrNoConsole)
	}
	ctrl := r.ladderControl()

	r.SetState(StateStopping)

	l := &Ladder{
		Plan:     r.Plan(),
		Proc:     proc,
		Ring:     r.ring,
		Console:  r.consoleSender(),
		Control:  ctrl,
		Clock:    r.clock,
		Notify:   r.notify,
		Log:      r.log,
		Note:     r.Note,
		Step:     r.stopStep,
		Progress: r.ingest.LastByte,
		Extended: &r.extendedUsec,
	}
	rep := l.Run(ctx, actor, reason)

	r.mu.Lock()
	if st, ok := l.Exit(); ok {
		r.exit, r.exited = st, true
	}
	r.rung, r.degraded = rep.FinalRung, rep.Degraded
	r.mu.Unlock()

	if rep.Exited {
		r.SetState(StateExited)
	} else {
		// The game outlived every rung. `failed` rather than `exited` is what
		// makes Restart=on-failure fire and an alert exist, which is the whole
		// point of distinguishing them.
		r.SetState(StateFailed)
	}

	r.mu.Lock()
	r.stopReport = rep
	r.mu.Unlock()

	if err := rep.Save(r.cfg.Paths.ShutdownLog, r.cfg.Paths.LastShutdown); err != nil {
		// The world is stopped; only the record failed. Logged loudly because
		// gvmd ingests that file and a gap in it is a shutdown nobody can
		// audit.
		r.log.Error("writing the shutdown report", "err", err)
		r.Note("-- gvm: could not write the shutdown report: %v", err)
	}
	return rep
}

// AwaitStop blocks until the shutdown ladder currently running has finished
// and written its report, and returns that report. It returns nil immediately
// when no ladder is running.
//
// It exists because `gvm run`'s teardown and a socket-initiated `stop` are two
// goroutines with no barrier between them: osProcess.Wait is a closed-channel
// broadcast, so the ladder's own successful `quit` wakes the supervisor in the
// same instant as the ladder's exit watcher, and the loser of that race has
// strictly more work left to do -- settled(), the reap, the state write and
// rep.Save, which fsyncs. A teardown that wins closes the ring the ladder is
// still annotating and drops the connection the operator is watching.
func (r *Runner) AwaitStop(ctx context.Context) *Report {
	r.mu.Lock()
	done := r.stopDone
	r.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopReport
}

// consoleSender is the runner as a console channel, or nil when there is no
// stream. A typed nil would give the ladder a non-nil interface that fails on
// every send instead of a rung it can honestly record as skipped.
func (r *Runner) consoleSender() ConsoleSender {
	if !r.launch.Console.Capturing() {
		return nil
	}
	return r
}

// stopStep records the rung for status.json and forwards it to whatever
// narration hook is installed.
//
// The recording is the point: status.json is the DB-free truth, and an
// operator watching a ninety second save wants "rung: console" rather than a
// spinner. The hook is optional; the record is not.
func (r *Runner) stopStep(rung, send string) {
	r.mu.Lock()
	r.rung = Rung(rung)
	hook := r.onStep
	r.mu.Unlock()
	if hook != nil {
		hook(rung, send)
	}
	r.WriteStatus()
}

// refusedReport records a shutdown that never started.
func (r *Runner) refusedReport(actor, reason string, err error) *Report {
	now := r.clock.Now()
	return &Report{
		Proto:       ReportProto,
		Instance:    r.cfg.Instance,
		Game:        r.cfg.Game,
		Unit:        r.cfg.Unit,
		Reason:      reason,
		Actor:       actor,
		StartedAt:   now,
		FinishedAt:  now,
		ExitCode:    -1,
		Err:         err.Error(),
		ConsoleTail: tailFrom(r.ring),
	}
}

// Stop implements the console socket's Backend: it runs the ladder, narrating
// each rung to the attached client, and returns the report as JSON.
//
// It blocks the connection's read goroutine for the ladder's whole duration by
// design. The fan-out keeps streaming console lines meanwhile, which is what
// an operator actually watches during a ninety second save.
func (r *Runner) Stop(ctx context.Context, actor, reason string, step func(rung, send string)) (json.RawMessage, error) {
	r.mu.Lock()
	prev := r.onStep
	r.onStep = step
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.onStep = prev
		r.mu.Unlock()
	}()

	rep := r.Shutdown(ctx, actor, reason)
	if rep.Err != "" && len(rep.Steps) == 0 {
		// The ladder never ran. The socket needs an error to map onto a code,
		// not a report that would render as a successful stop.
		return nil, errors.New(rep.Err)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return nil, err
	}
	return b, nil
}
