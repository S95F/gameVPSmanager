package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// FindingKind names a thing a sweep noticed.
//
// The set is closed and each value is a string an operator can grep for in
// journald, in `gvm doctor` output and in this file.
type FindingKind string

const (
	// FindingPhase is any phase whose severity is a problem. It exists so
	// that a sweep report can be counted without every consumer
	// re-implementing Phase.Severity.
	FindingPhase FindingKind = "phase"
	// FindingSpecDrift is a spec edited since the last apply: the drop-in's
	// header sha256 is not the sha256 of the YAML on disk. Section 20's
	// INST-005.
	FindingSpecDrift FindingKind = "spec_drift"
	// FindingNotApplied is a spec with no generated drop-in at all.
	FindingNotApplied FindingKind = "not_applied"
	// FindingForeignDropIn is a file at gvm's own drop-in path that gvm did
	// not write. It is never overwritten and always reported.
	FindingForeignDropIn FindingKind = "foreign_dropin"
	// FindingLocalDropIn is 90-local.conf. Section 5.3 promises gvm never
	// writes, reads or deletes it -- and section 20's INST-014 reports it, so
	// that a directive an operator cannot find in 10-gvm.conf is not
	// invisible.
	FindingLocalDropIn FindingKind = "local_dropin"
	// FindingStaleLease is a job that died holding the instance.
	FindingStaleLease FindingKind = "stale_lease"
	// FindingNotRunning is section 13.4's desired_running && !active. The
	// sweep REPORTS it; `gvm-guard.timer` is what acts on it.
	FindingNotRunning FindingKind = "not_running"
	// FindingHeld is an instance an operator stopped. A notice, not a
	// problem: it is doing what it was told.
	FindingHeld FindingKind = "held"
	// FindingFreezeArmed is a freeze lease present on disk (D-31). Normal
	// during a backup, and the thing to look at first when a world stops
	// persisting.
	FindingFreezeArmed FindingKind = "freeze_armed"
	// FindingObservation is a source the observer could not read.
	FindingObservation FindingKind = "observation"
)

// Finding is one thing worth telling somebody about.
type Finding struct {
	Instance string      `json:"instance"`
	Kind     FindingKind `json:"kind"`
	Severity Severity    `json:"severity"`
	Detail   string      `json:"detail"`
	Hint     string      `json:"hint,omitempty"`
}

// InstanceReport is one instance's whole sweep result.
type InstanceReport struct {
	Name        string      `json:"name"`
	Observation Observation `json:"observation"`
	Verdict     Verdict     `json:"verdict"`
	Findings    []Finding   `json:"findings,omitempty"`
}

// SweepReport is what one pass over every instance saw.
type SweepReport struct {
	At        time.Time        `json:"at"`
	Took      time.Duration    `json:"took"`
	Instances []InstanceReport `json:"instances"`
	// Findings is every instance's findings, flattened, so a caller counts
	// once rather than walking twice.
	Findings []Finding `json:"findings,omitempty"`
	// Problems are the sweep's own: an instance directory entry that is not a
	// legal name, a mirror write that failed. A mirror failure is HERE and
	// not an error return, because the sweep succeeded -- what failed is the
	// copy of it nobody makes a decision from.
	Problems []Problem `json:"problems,omitempty"`
}

// Counts summarises a report by severity.
func (r SweepReport) Counts() (ok, notice, problem int) {
	for _, ir := range r.Instances {
		switch ir.Verdict.Phase.Severity() {
		case SevOK:
			ok++
		case SevNotice:
			notice++
		default:
			problem++
		}
	}
	return
}

// Mirror is the optional database write.
//
// It is an interface with one method and a nil value that means "no
// database", which is section 14.6 expressed as a type: the sweep is a
// file-derived operation and Postgres is a place to copy the answer to, never
// a place to read it from. Nothing in this package ever calls a Mirror to
// DECIDE anything.
type Mirror interface {
	// RecordPhase writes one instance's observation to the mirror. Its error
	// is logged and discarded, so an implementation should not retry
	// internally for longer than a sweep interval.
	RecordPhase(ctx context.Context, r InstanceReport) error
}

// Reconciler observes every instance and reports what it saw.
//
// IT NEVER ACTUATES. There is no start, stop, restart, enable, disable or
// write anywhere in a sweep, and the type cannot express one: its view of
// systemd is a UnitReader, which has no method that changes anything, and its
// view of the filesystem is a Files, which has no method that writes.
//
// The reason is not purity. A reconciler that healed by itself would fight the
// operator who stopped an instance to repair a world by hand, would restart a
// server mid-`gvm rebuild` because it briefly looked down, and would do both
// at 3am with nobody watching. Healing is deliberate and it has an owner:
// `gvm-guard.timer` runs `gvm job run _guard`, which decides from spec.enabled
// and the hold file, records a job_runs row saying what it did and why, and is
// therefore something an operator can find afterwards (section 13.4).
//
// TestReconcilerNeverActuates drives a full sweep with drift present against a
// fake that implements the whole of platform.Systemd and counts its calls; the
// count is zero. testdata/reconciler-actuates is the compile-time half.
type Reconciler struct {
	// Obs is the observation seam. Everything the sweep learns comes through
	// it, so injecting a fake Files and a fake UnitReader is enough to drive
	// any host shape from a Windows test.
	Obs *Observer
	// Store is the optional mirror. Nil means no database, which is the
	// normal state on a box where Postgres has fallen over and the whole
	// point of the design is that nothing else changes.
	Store Mirror
	// Log is where warnings go. Nil is slog.Default.
	Log *slog.Logger
}

func (r *Reconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Reconciler) obs() *Observer {
	if r.Obs != nil {
		return r.Obs
	}
	return &Observer{}
}

// Sweep observes every instance once.
//
// The instance list comes from /etc/gvm/instances/*.yaml and NEVER from the
// database. That is section 1's control-plane-dead rule written as code: with
// Postgres down, `gvm status` must still enumerate every server on the box,
// and a sweep that asked the database for the list would return an empty one
// and report a host with eleven running worlds as having none.
//
// A failure to enumerate IS an error, because a sweep that found nothing and a
// sweep that could not look are the same report otherwise, and the second one
// must not be mistaken for a clean bill of health. A failure to MIRROR is not:
// it becomes a Problem and a warning, and the sweep continues.
func (r *Reconciler) Sweep(ctx context.Context) (SweepReport, error) {
	o := r.obs()
	start := o.now()
	rep := SweepReport{At: start}

	names, probs, err := List(o.files())
	if err != nil {
		return rep, err
	}
	rep.Problems = append(rep.Problems, probs...)

	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		ir := InstanceReport{Name: name}
		obs, err := o.Observe(ctx, name)
		if err != nil {
			// Observe only refuses a name it cannot derive paths from, and
			// List already filtered those. Recording it rather than aborting
			// keeps one strange file from hiding ten healthy instances.
			rep.Problems = append(rep.Problems, Problem{Source: "observe", Detail: err.Error()})
			continue
		}
		ir.Observation = obs
		ir.Verdict = Derive(obs)
		ir.Findings = Findings(obs, ir.Verdict)
		rep.Instances = append(rep.Instances, ir)
		rep.Findings = append(rep.Findings, ir.Findings...)

		r.mirror(ctx, ir, &rep)
	}
	rep.Took = o.now().Sub(start)
	return rep, nil
}

// mirror copies one report to the database, best effort.
//
// Section 14.6: no operation blocks on the database. The warning names the
// instance because a mirror that is failing for one row and working for the
// rest is a different problem from Postgres being down, and a message that
// said only "mirror write failed" would not distinguish them.
func (r *Reconciler) mirror(ctx context.Context, ir InstanceReport, rep *SweepReport) {
	if r.Store == nil {
		return
	}
	if err := r.Store.RecordPhase(ctx, ir); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		r.log().Warn("lifecycle: mirror write failed; the sweep is unaffected",
			"instance", ir.Name, "phase", ir.Verdict.Phase, "err", err)
		rep.Problems = append(rep.Problems, Problem{
			Source: "mirror", Detail: ir.Name + ": " + err.Error(),
		})
	}
}

// DesiredRunningAndDown is section 13.4's `desired_running` with the unit
// observed down -- minus the lease, which is the one term its two callers
// legitimately disagree about.
//
// It is exported so that there is exactly ONE copy of the predicate. The guard
// timer's sweep (internal/jobs) and FindingNotRunning above both have to
// answer "should this be running, and is it not", and a second derivation
// would mean `gvm doctor` reporting a box healthy while the guard started
// something, or the reverse, the first night one of the two grew a term.
//
// The lease clause stays with each caller because the two needs are different
// and both are right. A finding is a SENTENCE, and "a job is working on it" is
// the better sentence for any lease at all, so Findings suppresses on all of
// them. The sweep ACTS, and a lease whose holder is provably gone is proof
// that nobody is working on it -- which is the whole case gvm-guard.timer
// exists for, since a `defer` does not survive SIGKILL.
//
// The unit-file-enabled term of section 13.4 is absent because the
// platform.Systemd seam does not report it yet; the terms that can be seen are
// reported honestly rather than guessed at.
func (o Observation) DesiredRunningAndDown() bool {
	return o.Enabled && !o.Hold && o.UnitChecked && !o.Unit.Running() &&
		o.Unit.ActiveState != "activating" &&
		!o.NeedsOperator() &&
		o.DropIn.SpecSHA256 != ""
}

// NeedsOperator reports whether the unit's last failure is one no restart can
// fix, so the guard must leave it down and say why rather than loop it: an exit
// code in RestartPreventExitStatus (64/65/66/78/79), or systemd's own
// start-limit-hit. It is the Go twin of gvm@.service's RestartPreventExitStatus
// plus the one state systemd adds, and TestNeedsOperatorMatchesRestartPrevent
// ExitStatus holds the two together.
//
// A unit that is currently active is never "needs operator": its
// ExecMainStatus may be a stale code from a previous exec, and a server that is
// up and serving does not need anybody.
func (o Observation) NeedsOperator() bool {
	if !o.UnitChecked || o.Unit.ActiveState == "active" {
		return false
	}
	if o.Unit.Result == "start-limit-hit" {
		return true
	}
	for _, code := range RestartPreventExitStatus {
		if o.Unit.ExecMainStatus == code {
			return true
		}
	}
	return false
}

// Findings is the pure half of the sweep: what is worth saying about one
// observation.
//
// It is exported and pure so that `gvm doctor` and `gvm status` produce the
// same list from the same facts. Every entry states a condition; none of them
// implies an action this package will take.
func Findings(o Observation, v Verdict) []Finding {
	var out []Finding
	add := func(k FindingKind, s Severity, detail, hint string) {
		out = append(out, Finding{Instance: o.Name, Kind: k, Severity: s, Detail: detail, Hint: hint})
	}

	if v.Phase.Severity() == SevProblem {
		add(FindingPhase, SevProblem, string(v.Phase)+": "+v.Reason, v.Hint)
	}
	if !o.SpecPresent {
		// Nothing below this line is meaningful without a spec, and an
		// absent instance has no drift to report.
		return out
	}

	switch {
	case o.DropInPresent && !o.DropIn.Generated:
		add(FindingForeignDropIn, SevProblem,
			dropInPath(o.Name)+" was not written by gvm and is not being overwritten",
			"move your directives into "+localDropIn)
	case o.DropIn.SpecSHA256 == "":
		add(FindingNotApplied, SevNotice,
			"no apply has converged "+specPath(o.Name), "gvm apply "+o.Name)
	case !strings.EqualFold(o.DropIn.SpecSHA256, o.SpecSHA256):
		// The header carries the sha256 of the YAML apply last read, so this
		// is exactly section 20's INST-005 -- "spec edited, never applied" --
		// answerable with no database and no memory of what gvm did last.
		add(FindingSpecDrift, SevProblem,
			"the spec has been edited since the last apply (spec "+short(o.SpecSHA256)+
				" != applied "+short(o.DropIn.SpecSHA256)+")",
			"gvm apply --diff "+o.Name)
	}

	if o.LocalDropIn {
		add(FindingLocalDropIn, SevNotice,
			localDropIn+" exists; it is yours, and gvm does not read or write it", "")
	}
	if o.LeaseState == LeaseStale || o.LeaseUnreadable {
		add(FindingStaleLease, SevProblem,
			"a job lease is present and its holder is gone", "gvm job clear "+o.Name)
	}
	if o.FreezeLease {
		// Normal during a backup and the first thing to look at when a world
		// stops persisting, so it is a notice with a sentence rather than
		// nothing at all.
		add(FindingFreezeArmed, SevNotice,
			"a freeze lease is armed; autosave may be off while a backup runs", "")
	}
	if o.Hold {
		add(FindingHeld, SevNotice, holdReason(o), "gvm start "+o.Name)
	}
	// Section 13.4's desired_running, minus the unit-file-enabled term, which
	// the platform.Systemd seam does not report yet. Reporting the two terms
	// it can see is honest; acting on them is the guard job's and not this
	// package's under any circumstance.
	//
	// The three exclusions are all "there is a better sentence for this".
	// An unapplied instance has no unit to be down; a leased one is being
	// worked on; an unchecked manager means the unit state is not evidence of
	// anything. Reporting all four conditions for one instance would make the
	// finding list a place operators skim rather than read.
	if o.DesiredRunningAndDown() &&
		!o.LeaseUnreadable && (o.LeaseState == "" || o.LeaseState == LeaseNone) {
		add(FindingNotRunning, SevProblem,
			"enabled, not held, and the unit is "+o.Unit.ActiveState,
			"gvm start "+o.Name)
	}
	// A unit that needs a human (exit 64/65/66/78/79 or start-limit-hit) gets a
	// sentence saying so, with the exit's hint, so the operator knows the guard
	// has deliberately left it down rather than missed it.
	if o.NeedsOperator() {
		add(FindingPhase, SevProblem,
			"the guard will not restart this: "+v.Reason, v.Hint)
	} else if v.Phase == PhaseRunning && o.Unit.NRestarts > 0 {
		// A past restart on a server that is now ready is history, not a loop:
		// systemd's NRestarts is cumulative until an explicit start/stop or
		// reset-failed, so this is a Note that never counts as a problem.
		add(FindingObservation, SevNotice,
			fmt.Sprintf("restarted %s earlier and is now ready; last exit %d (%s)",
				restartCount(o.Unit.NRestarts), o.Unit.ExecMainStatus,
				exitMeaning(o.Unit.ExecMainStatus)),
			"")
	}
	for _, p := range o.Problems {
		add(FindingObservation, SevNotice, p.Source+": "+p.Detail, "")
	}
	return out
}

// Watch sweeps on every tick until ctx is done.
//
// The tick is a parameter rather than an interval so that a test drives it
// directly and never sleeps. A watch loop with a real ticker inside it is a
// test that takes as long as the interval and then flakes on a slow machine.
//
// emit is called with each report, including the ones with no findings: a
// consumer that only heard about problems could not tell a healthy sweep from
// a watcher that had silently stopped.
func (r *Reconciler) Watch(ctx context.Context, tick <-chan time.Time, emit func(SweepReport)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-tick:
			if !ok {
				return nil
			}
			rep, err := r.Sweep(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// A sweep that could not enumerate is worth a warning and not
				// worth stopping for: /etc/gvm may be momentarily unreadable
				// during a package upgrade, and a watcher that exited would
				// have to be noticed by a human to come back.
				r.log().Warn("lifecycle: sweep failed", "err", err)
				continue
			}
			if emit != nil {
				emit(rep)
			}
		}
	}
}

// WatchEvery is Watch with a real ticker, for production callers.
func (r *Reconciler) WatchEvery(ctx context.Context, every time.Duration, emit func(SweepReport)) error {
	t := time.NewTicker(every)
	defer t.Stop()
	return r.Watch(ctx, t.C, emit)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
