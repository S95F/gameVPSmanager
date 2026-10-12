package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// RunningGuard is update.sh's load-bearing comment, enforced by a type.
//
//	# Also runs unattended every night ... so no failure here may leave the
//	# server stopped.
//
// The bash suite honours that with `|| warn` on every fallible line and a
// `systemctl start` at the end that runs whatever happened. That works exactly
// as long as every future edit remembers to write `|| warn`, and the one line
// that forgets is a line nobody notices until 05:03 on a Tuesday.
//
// Here the promise is a `defer`. RunningGuard captures whether the instance was
// active when the job began and guarantees it is active again when the job
// ends, whatever happened in between. Restore is idempotent and is registered
// with defer as the FIRST statement after acquisition, before any fallible
// work, so there is no window where an early return skips it:
//
//	g, err := jobs.Guard(ctx, env)   // records wasActive; no side effects
//	if err != nil { return err }
//	defer g.Restore(ctx)             // <- before anything can fail
//
// Three failure modes, and a guard that only covers the first one is worthless:
//
//   - an error return: ordinary defer semantics.
//   - a panic: the deferred Restore runs while the panic is unwinding, BEFORE
//     the recover that records the failure, because defers run last-registered
//     first and the engine registers its recover earlier. Restore also recovers
//     a panic of its OWN, so a broken seam cannot swallow the breadcrumb.
//   - a cancelled context: this is the one a naive implementation gets wrong.
//     systemd hits TimeoutStartSec, sends SIGTERM, the job's context is
//     cancelled -- and a Restore that used that context could not issue the
//     start. So the restore detaches with context.WithoutCancel and takes a
//     budget of its own. The job is over; the server still has to come back.
//
// What the `defer` cannot survive is SIGKILL of the job process -- the OOM
// killer, `systemctl kill`, power loss. That is what `gvm-guard.timer` is for
// (section 13.4), and the breadcrumb this writes is what it reads.
type RunningGuard struct {
	env *Env
	log *slog.Logger
	res Restoration
	// heldAtEntry is the hold as it was BEFORE the job ran, so restore can
	// tell an operator's marker from one that appeared during the job. It is
	// belt and braces over Request.NoHold: even a future stop path that wrote
	// a hold cannot disarm the guard with this recorded.
	heldAtEntry bool
	done        bool
	budget      time.Duration
}

// Restoration is what the guard observed and what it did about it.
//
// LeftStopped is the field this whole package exists to keep false. It is
// `job_runs.left_stopped`, and section 14.4's comment on that column is
// "true here is a doctor error".
type Restoration struct {
	// Armed is whether a guard was installed at all. False for a host-level
	// job, which has no single instance to guard.
	Armed bool `json:"armed"`
	// WasActive is the answer to `systemctl is-active` at entry, widened to
	// include `activating` -- see Guard.
	WasActive bool `json:"was_active"`
	// Unknown records that the entry observation FAILED. The guard then
	// behaves as if the instance had been active, because a start is
	// idempotent and leaving a world down is not.
	Unknown bool `json:"unknown,omitempty"`
	// ActiveAfter is the answer at exit.
	ActiveAfter bool `json:"active_after"`
	// Started is whether the guard itself issued the start.
	Started bool `json:"started,omitempty"`
	// Held records that an operator hold appeared while the job ran, so the
	// guard deliberately did NOT start the instance.
	Held bool `json:"held,omitempty"`
	// Degraded is whether the guard had to do something, or could not fully
	// account for the result.
	Degraded bool `json:"degraded,omitempty"`
	// LeftStopped is the unforgivable outcome: the instance was serving when
	// the job began and is not now.
	LeftStopped bool `json:"left_stopped,omitempty"`
	// Reasons is the chain section 13.4 asks to record: what the guard found,
	// what it tried, and why it did not work.
	Reasons []string `json:"reasons,omitempty"`
	// Note is the bash suite's own line, present only when it applies.
	Note string `json:"note,omitempty"`
	// Breadcrumb is the path of the marker the guard wrote, if it wrote one.
	Breadcrumb string `json:"breadcrumb,omitempty"`
}

// Supervisor is the job layer's entire view of whether a server is up and how
// to put it back.
//
// Four methods, and the reason it is a seam rather than a call into
// internal/lifecycle is rule 5 of this project: the never-leaves-stopped test
// has to run on Windows, where there is no systemd to ask. A fake implementing
// these four is a fake systemd, and the guard's whole decision table becomes
// an ordinary table test on the development machine.
//
// There is deliberately no Enable, Disable or Mask here. A job may stop and
// start a server; changing whether it is SUPPOSED to run is a change to intent,
// and intent has exactly one write path (internal/reconcile).
type Supervisor interface {
	// Active reports whether the instance is serving or on its way to
	// serving.
	Active(ctx context.Context, name string) (bool, error)
	// Held reports whether /run/gvm/<name>/hold is present: an operator
	// stopped this instance on purpose and the guard must not fight them.
	Held(ctx context.Context, name string) (bool, error)
	// Start brings it up. It is idempotent: starting a running unit is a
	// no-op, which is what lets the guard start on an inconclusive
	// observation rather than guessing.
	Start(ctx context.Context, name string) error
	// Stop runs the shutdown ladder and reports the rung it ended on.
	Stop(ctx context.Context, name string, budget time.Duration) (lifecycle.Outcome, error)
}

// Host is the privileged surface, and it is exactly hostops.Do.
//
// One method, for the same reason internal/lifecycle and internal/reconcile
// both declare the same interface: there is no second method through which a
// privileged operation could be reached, so the enumerable surface stays
// enumerable.
type Host interface {
	Do(ctx context.Context, req hostops.Request) (hostops.Result, error)
}

// The four deferred legs of a job, and their sum.
//
// A cancelled job runs them in sequence -- the step unwind, the lease release,
// the running guard's restore, the terminal audit row -- each on its own
// detached context, and the whole chain races systemd's TimeoutStopSec with
// SendSIGKILL=yes behind it. They used to budget up to twenty-one minutes
// between them against a 600-second stop timeout, so SIGKILL could land inside
// the guard's Super.Start: no breadcrumb, no terminal job_runs row, and the
// evidence an operator needs is exactly what the chain was spending the time
// to write. packaging's TestTheDeferredChainFitsInsideTimeoutStopSec is the
// assertion that keeps the two numbers in agreement, in the same shape
// internal/spec already asserts the shutdown ladder against the instance
// unit's.
//
// The split gives the restore the largest share because it is the leg that
// genuinely waits: gvm@.service is Type=notify, so its start job does not
// complete until READY=1, and hostops' job() waits for JobRemoved because
// "that is the whole point". For a game whose readiness.hard_deadline is an
// hour -- Project Zomboid with a large workshop list -- that wait is longer
// than any budget here, which is why RunningGuard.restore writes its marker
// BEFORE the start rather than after it. The budget bounds the wait; the
// marker is what survives it.
const (
	// UnwindBudget bounds every step Undo put together. They are bounded
	// things -- a thaw rung, an unmask -- and the shipped rung waits are
	// 15-45s.
	UnwindBudget = 150 * time.Second
	// ClearLeaseBudget bounds one audited unlink.
	ClearLeaseBudget = 15 * time.Second
	// DefaultRestoreBudget bounds the guard's restore.
	//
	// Finite, for D-12's reason: an infinite wait inside a deferred function
	// is a job that never exits, which systemd eventually SIGKILLs -- turning
	// the one code path that was supposed to save the situation into the thing
	// that prevents the breadcrumb from being written. And a job that sat in a
	// deferred function for an hour would hold the instance lock against the
	// next nightly.
	DefaultRestoreBudget = 285 * time.Second
	// FinishBudget bounds the terminal audit row and the run record.
	FinishBudget = 30 * time.Second

	// DeferredChainBudget is what the four legs may spend between them, and
	// it is what packaging asserts against TimeoutStopSec.
	DeferredChainBudget = UnwindBudget + ClearLeaseBudget + DefaultRestoreBudget + FinishBudget
)

// Guard records whether the instance is active. It has no side effects.
//
// "No side effects" is part of the contract and not an implementation detail:
// this runs before the job has decided to do anything, including before the
// decision that most nights means doing nothing at all (D-43). A guard that
// wrote a file here would make every skipped nightly a write.
//
// A failed observation is NOT a failed acquisition. It records Unknown and
// treats the instance as having been active, because the two mistakes are not
// symmetric: starting a server that was already running is a no-op, and failing
// to start one that was down is the outage this file exists to prevent.
func Guard(ctx context.Context, env *Env) (*RunningGuard, error) {
	if env == nil {
		return nil, errors.New("jobs: the running guard needs an environment")
	}
	if env.Instance == "" {
		// A host-level job has no single instance to guard. It is not an
		// error -- `_guard` itself is one -- so the caller gets a guard that
		// is honestly unarmed rather than a nil it has to test for.
		return &RunningGuard{env: env, log: env.logger(), res: Restoration{}}, nil
	}
	if err := instance.ValidateName(env.Instance); err != nil {
		return nil, err
	}
	if env.Super == nil {
		return nil, fmt.Errorf("jobs: %s: the running guard needs a supervisor; without one "+
			"there is no way to put the server back and no job may run", env.Instance)
	}
	g := &RunningGuard{
		env:    env,
		log:    env.logger(),
		budget: env.restoreBudget(),
		res:    Restoration{Armed: true},
	}
	active, err := env.Super.Active(ctx, env.Instance)
	if err != nil {
		g.res.Unknown = true
		g.res.WasActive = true
		g.res.Degraded = true
		g.reason("could not tell whether %s was running at entry (%v); treating it as running, "+
			"because starting one that is already up is a no-op and leaving one down is not",
			env.Instance, err)
		return g, nil
	}
	g.res.WasActive = active
	// Still side-effect-free: this is a read, like Active above. An error is
	// not recorded as degraded here, because not knowing the hold at entry
	// only costs the guard its ability to distinguish an operator's marker
	// from a forged one, and restore re-reads it anyway.
	if held, herr := env.Super.Held(ctx, env.Instance); herr == nil {
		g.heldAtEntry = held
	}
	return g, nil
}

// Armed reports whether this guard will restore anything.
func (g *RunningGuard) Armed() bool { return g != nil && g.res.Armed }

// WasActive is the answer the guard captured at entry.
func (g *RunningGuard) WasActive() bool { return g != nil && g.res.WasActive }

// Result is what the guard did. It is meaningful only after Restore.
func (g *RunningGuard) Result() Restoration {
	if g == nil {
		return Restoration{}
	}
	return g.res
}

// Restore puts the instance back, and is idempotent.
//
// It is nil-safe and double-call-safe on purpose: the engine registers it with
// defer and a caller writing a job by hand will reasonably also call it on the
// success path, exactly as internal/audit's Span.Done is idempotent for the
// same reason.
func (g *RunningGuard) Restore(ctx context.Context) {
	if g == nil || g.done {
		return
	}
	g.done = true
	if !g.res.Armed {
		return
	}

	// Detached, and this is the single most important line in the file. By the
	// time a deferred Restore runs, the job's context may already be cancelled
	// -- systemd reached TimeoutStartSec and sent SIGTERM, or an operator
	// pressed ^C. Using that context would make every one of those cases a
	// server left stopped, which is precisely the hazard the guard exists for.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.budget)
	defer cancel()

	// And the guard's own panic cannot be allowed to swallow the breadcrumb.
	// A Supervisor is a seam, and a fake or a future implementation that
	// panics must still leave the marker `gvm-guard.timer` reads.
	defer func() {
		if p := recover(); p != nil {
			g.reason("the running guard itself panicked: %v", p)
			g.res.Degraded = true
			if g.res.WasActive && !g.res.ActiveAfter {
				// Only then, because the breadcrumb's text is a statement of
				// fact and `gvm-guard.timer` is the thing that acts on it. A
				// panic after the instance was confirmed up is a gvm defect
				// and not an outage.
				g.res.LeftStopped = true
				g.breadcrumb(rctx)
			}
			g.log.Error("the running guard panicked",
				"instance", g.env.Instance, "job", g.env.Job, "panic", fmt.Sprint(p))
		}
	}()

	g.restore(rctx)
}

func (g *RunningGuard) restore(ctx context.Context) {
	name := g.env.Instance

	active, err := g.env.Super.Active(ctx, name)
	if err != nil {
		// Same asymmetry as at entry: not knowing is a reason to start, not a
		// reason to walk away.
		g.reason("could not tell whether %s is running at exit (%v)", name, err)
		g.res.Degraded = true
		active = false
	}
	g.res.ActiveAfter = active

	if !g.res.WasActive {
		// Section 13.4: a no-op, plus the bash suite's own note, printed only
		// when it applies. gvm deliberately does NOT start a server that was
		// stopped before the job -- update.sh does, and that is the one place
		// this diverges from it, because a box where `gvm stop` is honoured is
		// a box where a maintenance window works.
		if active {
			g.res.Note = fmt.Sprintf("the server was stopped before this %s; it is running now",
				g.env.Kind)
			g.log.Info(g.res.Note, "instance", name, "job", g.env.Job)
		}
		return
	}

	if active {
		// The job either never stopped it or put it back itself. Nothing to
		// do, and nothing to say: this is most nights.
		g.clearBreadcrumb(ctx)
		return
	}

	// It was serving and it is not. Before starting it, check whether a human
	// asked for it to be down.
	if held, herr := g.env.Super.Held(ctx, name); herr != nil {
		g.reason("could not read the operator hold for %s (%v); starting anyway, because a "+
			"hold that cannot be read is not a hold", name, herr)
		g.res.Degraded = true
	} else if held && !g.heldAtEntry {
		// An operator ran `gvm stop` while the job was running. They win: a
		// guard that fought an explicit stop would make maintenance
		// impossible, and `gvm-guard.timer` honours the same marker for the
		// same reason. No breadcrumb -- the hold would suppress the timer
		// anyway, and a marker nobody acts on is noise in /run.
		//
		// `!heldAtEntry` is what makes this a claim about a HUMAN rather than
		// about a file. A hold that was already there at entry cannot be
		// evidence of a decision taken during the job -- and if it was there
		// at entry the instance was not serving either, so WasActive is false
		// and this branch is unreachable by that route. What it really
		// excludes is a hold the job itself wrote, which is the failure that
		// left worlds down overnight with left_stopped=false and no
		// breadcrumb.
		g.res.Held = true
		g.res.Degraded = true
		g.reason("%s was running when this %s began and is stopped now, but an operator hold "+
			"is present: %s says a human wants it down, so the guard left it down",
			name, g.env.Kind, instance.PathsFor(name).Hold)
		g.log.Warn("the running guard yielded to an operator hold",
			"instance", name, "job", g.env.Job, "kind", string(g.env.Kind))
		return
	}

	g.res.Degraded = true
	g.reason("%s was running when this %s began and was stopped when it ended; the guard "+
		"started it", name, g.env.Kind)

	// The marker goes down BEFORE the start, not after it, and this is the
	// one write the arithmetic above cannot replace.
	//
	// Super.Start waits for the systemd job to complete, and gvm@.service is
	// Type=notify -- so for a game whose readiness.hard_deadline is an hour
	// the start job does not complete until the world is up. The guard's
	// budget cuts that off, and if TimeoutStopSec gets there first, SIGKILL
	// lands inside this wait: no breadcrumb, no terminal job_runs row, and an
	// instance whose only trace is a journal line. A marker written here is
	// true while it is written, true if this process is killed, and removed
	// as soon as the start is confirmed -- so the sweep has a reason that
	// names the job instead of the generic "enabled, not held, and the unit
	// is inactive".
	g.reason("the guard is starting %s now; no terminal row for this run means the start was "+
		"still in flight when the job was killed", name)
	g.breadcrumb(ctx)

	if err := g.env.Super.Start(ctx, name); err != nil {
		g.fail(ctx, fmt.Errorf("the guard could not start %s: %w", name, err))
		return
	}
	g.res.Started = true

	// Readiness is optional and bounded by whoever supplied it. The unit
	// being up is what the guard promises; the world being joinable is what
	// `gvm start --wait` promises, and conflating them would make a deferred
	// function wait out a 40-minute workshop boot.
	if g.env.WaitReady != nil {
		if err := g.env.WaitReady(ctx, name); err != nil {
			g.reason("%s started but did not report ready: %v", name, err)
		}
	}

	// Confirm, rather than trust the start's return. A start that returned nil
	// and left the unit inactive is exactly the exit-code-64/65 case from
	// section 7.8, where Restart=on-failure is deliberately PREVENTED -- so
	// nothing else is going to try again, and this is the moment to say so.
	after, aerr := g.env.Super.Active(ctx, name)
	switch {
	case aerr != nil:
		// The marker written before the start STAYS. It says a start was in
		// flight and nobody could confirm it, which is exactly true, and
		// gvm-guard.timer re-derives the answer from scratch within five
		// minutes.
		// The start returned success and the confirmation could not be read.
		// ActiveAfter stays true, because claiming an outage gvm cannot see
		// would make every D-Bus hiccup a `failed` nightly -- and NO
		// breadcrumb is written, because the file says "left this instance
		// stopped" and that would be a false statement in a file an operator
		// reads. gvm-guard.timer sweeps every five minutes and re-derives the
		// answer from scratch, which is the right check for a result nobody
		// could confirm.
		g.reason("started %s and could not confirm it is running (%v); gvm-guard.timer will "+
			"re-check within 5 minutes", name, aerr)
		g.res.ActiveAfter = true
	case !after:
		g.fail(ctx, fmt.Errorf("started %s and it is not running; systemd accepted the job and "+
			"the unit is not active (see `journalctl -u %s`)", name, instance.UnitName(name)))
	default:
		g.res.ActiveAfter = true
		g.log.Warn("the running guard restarted an instance a job had left stopped",
			"instance", name, "job", g.env.Job, "kind", string(g.env.Kind),
			"reasons", strings.Join(g.res.Reasons, "; "))
		g.clearBreadcrumb(ctx)
	}
}

// fail is the terminal case of section 13.4: the start itself did not work.
//
// state='failed', left_stopped=true, the breadcrumb, and slog.Error -- in that
// order, and the breadcrumb before the log line because the breadcrumb is what
// a machine acts on and the log line is what a human reads afterwards.
func (g *RunningGuard) fail(ctx context.Context, err error) {
	g.res.ActiveAfter = false
	g.res.LeftStopped = true
	g.res.Degraded = true
	g.reason("%v", err)
	g.breadcrumb(ctx)
	g.log.Error("a job left a server stopped",
		"instance", g.env.Instance, "job", g.env.Job, "kind", string(g.env.Kind),
		"left_stopped", true, "err", err,
		"recovery", "gvm-guard.timer runs within 5 minutes")
}

// breadcrumb writes /run/gvm/<name>/degraded.
//
// Through hostops rather than with os.WriteFile, because the marker has a
// consequence -- `gvm-guard.timer` acts on it -- and because routing it through
// the privileged chokepoint means the audit row that says a world was left down
// is written by the same call that left the marker. A failure to write it is
// recorded and never fatal: the guard timer's own check (spec.enabled, unit
// enabled, no hold) catches the instance anyway, and refusing to return from a
// deferred function because /run is full would be the worse outcome.
func (g *RunningGuard) breadcrumb(ctx context.Context) {
	if g.env.Host == nil {
		g.reason("no privileged surface was configured, so no breadcrumb was written to %s",
			instance.PathsFor(g.env.Instance).Degraded)
		return
	}
	body := FormatDegraded(g.env.clock(), g.env.Kind, g.env.Slug, g.res.Reasons)
	out, err := g.env.Host.Do(ctx, hostops.Request{
		Verb: hostops.VerbFileWrite, Actor: g.env.Actor, Instance: g.env.Instance,
		File: hostops.FileDegraded, Body: body, RequestID: g.env.RequestID,
	})
	if err != nil {
		g.reason("could not write the degraded marker: %v", err)
		return
	}
	g.res.Breadcrumb = out.Path
}

// clearBreadcrumb removes a marker whose condition is over.
//
// Only on a path where the instance is active at exit AND was active at entry:
// a prune running against a deliberately stopped instance must not clear the
// marker last night's failed update left behind. Removing a file that is not
// there is a no-op in hostops, so there is no "does it exist" round trip.
func (g *RunningGuard) clearBreadcrumb(ctx context.Context) {
	if g.env.Host == nil || !g.res.WasActive {
		return
	}
	if _, err := g.env.Host.Do(ctx, hostops.Request{
		Verb: hostops.VerbFileRemove, Actor: g.env.Actor, Instance: g.env.Instance,
		File: hostops.FileDegraded, RequestID: g.env.RequestID,
	}); err != nil {
		g.reason("could not clear the degraded marker: %v", err)
	}
}

func (g *RunningGuard) reason(format string, args ...any) {
	g.res.Reasons = append(g.res.Reasons, fmt.Sprintf(format, args...))
}

// FormatDegraded renders /run/gvm/<name>/degraded.
//
// Human first, one record per line, newest fact last -- the same shape as
// lifecycle.FormatHold, because an operator who finds both with `cat` should
// not have to learn two formats. The reason chain is included in full: the
// whole value of this file at 07:00 is that it says WHY, and a marker that
// only said "degraded" would send the reader to journald for the sentence that
// was already known when the file was written.
func FormatDegraded(at time.Time, kind lifecycle.JobKind, slug string, reasons []string) []byte {
	var b strings.Builder
	b.WriteString(at.UTC().Format(time.RFC3339))
	b.WriteString(" ")
	b.WriteString(string(kind))
	if slug != "" {
		b.WriteString(" (")
		b.WriteString(slug)
		b.WriteString(")")
	}
	b.WriteString(" left this instance stopped\n")
	for _, r := range reasons {
		b.WriteString("  ")
		b.WriteString(sanitise(r))
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// sanitise strips the control characters a reason chain can pick up from a
// subprocess's output.
//
// Not because anything parsing this file would be fooled, but because an
// operator is going to `cat` it: an escape sequence from a mod's stderr is a
// terminal control sequence in their terminal, and a newline would make the
// second half look like a second record.
func sanitise(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			// Dropped, not escaped.
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// LifecycleSupervisor is the production Supervisor: internal/lifecycle's
// observer for the reads and its manager for the writes.
//
// It is thin on purpose. Every rule about whether a start is allowed, what a
// stop records and how a hold is written already lives in internal/lifecycle,
// and a job layer that re-decided any of it would be a second precondition
// table disagreeing with the one `gvm status` printed a second earlier.
type LifecycleSupervisor struct {
	// Obs answers the reads. Required.
	Obs *lifecycle.Observer
	// Mgr performs the verbs. Required.
	Mgr *lifecycle.Manager
	// Actor is who the manager records the start and stop as -- the timer, or
	// the operator who typed the command.
	Actor audit.Actor
	// Reason reaches the audit row and, for a stop, the hold file.
	Reason string
	// Force overrides an overridable refusal. A job sets it for its own stop:
	// it already holds the instance lease, and lifecycle's own precondition
	// table refuses a start while a live lease exists -- which is this job's
	// lease.
	Force bool
}

// Active reports whether the unit is up or coming up.
//
// `activating` counts, and that is a deliberate widening of what
// `systemctl is-active --quiet` (and therefore update.sh) would say. Project
// Zomboid's first boot with a large workshop list takes 40-60 minutes and the
// unit is `activating` for all of it. A guard that read that as "not running"
// would decide the instance had been down at entry, and then a nightly that
// fired mid-boot would leave it down.
func (s LifecycleSupervisor) Active(ctx context.Context, name string) (bool, error) {
	o, err := s.observe(ctx, name)
	if err != nil {
		return false, err
	}
	if !o.UnitChecked {
		return false, fmt.Errorf("jobs: could not read the state of %s", instance.UnitName(name))
	}
	switch o.Unit.ActiveState {
	case "active", "activating", "reloading", "deactivating":
		// deactivating counts too: the server is still there, still saving,
		// and a guard that called it stopped would issue a start into a
		// shutdown.
		return true, nil
	}
	return false, nil
}

// Held reports whether an operator stopped this instance on purpose.
func (s LifecycleSupervisor) Held(ctx context.Context, name string) (bool, error) {
	o, err := s.observe(ctx, name)
	if err != nil {
		return false, err
	}
	return o.Hold, nil
}

// Start brings the instance up and clears the hold.
func (s LifecycleSupervisor) Start(ctx context.Context, name string) error {
	if s.Mgr == nil {
		return errors.New("jobs: no lifecycle manager configured")
	}
	_, err := s.Mgr.Do(ctx, lifecycle.Request{
		Verb: lifecycle.VerbStart, Instance: name, Actor: s.Actor,
		Reason: s.Reason, Force: s.Force,
	})
	return err
}

// Stop runs the shutdown ladder.
func (s LifecycleSupervisor) Stop(ctx context.Context, name string, budget time.Duration) (lifecycle.Outcome, error) {
	if s.Mgr == nil {
		return lifecycle.Outcome{}, errors.New("jobs: no lifecycle manager configured")
	}
	res, err := s.Mgr.Do(ctx, lifecycle.Request{
		Verb: lifecycle.VerbStop, Instance: name, Actor: s.Actor,
		Reason: s.Reason, Budget: budget, Force: s.Force,
		// A JOB's stop, so no hold. /run/gvm/<n>/hold means a human wants
		// this instance down, and both the RunningGuard below and
		// gvm-guard.timer stand aside for it -- so a job that wrote one would
		// forge the one marker that disarms its own recovery. `gvm stop` from
		// an operator still writes it.
		NoHold: true,
	})
	if err != nil {
		return lifecycle.Outcome{}, err
	}
	if res.Shutdown == nil {
		// The manager refuses a stop with no ladder, so this is unreachable
		// through Do; it is checked because an Outcome with an empty rung is
		// an audit row that cannot say whether the world was saved.
		return lifecycle.Outcome{}, lifecycle.ErrNoLadder
	}
	return *res.Shutdown, nil
}

func (s LifecycleSupervisor) observe(ctx context.Context, name string) (lifecycle.Observation, error) {
	if s.Obs == nil {
		return lifecycle.Observation{}, errors.New("jobs: no lifecycle observer configured")
	}
	return s.Obs.Observe(ctx, name)
}
