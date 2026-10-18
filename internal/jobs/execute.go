package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/avail"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// The engine, and the ordering that is the whole design.
//
// Every job in gvm is the same five things in the same order:
//
//	1. take the instance lock              -- so two nightlies cannot overlap
//	2. ARM THE RUNNING GUARD               -- before anything that can fail
//	3. take the lease                       -- so `gvm start` refuses meanwhile
//	4. run the steps                        -- the only part that differs
//	5. unwind: undo, release, restore, record
//
// Step 2 being where it is, and not one line later, is the point of this file.
// The hazard is a failure between "stopped it" and "meant to start it again",
// and a guard installed after the first fallible call has a window exactly as
// wide as that call. So the guard is armed before the lease is written, before
// the audit span is closed over, before the first step is built -- and the
// deferred unwind below is registered in the one order that makes the guard's
// start succeed: undo the steps, release the lease (lifecycle REFUSES a start
// while a live lease holds the instance -- including gvm's own), restore the
// server, then record what happened.

// DefaultLeaseFor is the lease's self-expiry backstop.
//
// It matches gvm-job@.service's TimeoutStartSec=14400 (section 7.5) on
// purpose: the lease must outlive every legitimate run of every kind -- a
// 3600s Project Zomboid workshop boot inside a rebuild is the worst legitimate
// case -- and must not outlive the unit that holds it, because a deadline
// shorter than the job would let `gvm start` into a tree the job still has
// open, and one much longer would leave a dead job's lease refusing starts for
// no reason. lifecycle.Lease.Classify only consults the deadline when liveness
// cannot be established at all, so this is the last line of the chain rather
// than the first.
const DefaultLeaseFor = 4 * time.Hour

// ErrSkipJob is the sentinel a step returns when the work is not needed.
//
// It is not a failure and it is not an error the operator should see: it is
// D-43, "no Steam build change, no content change, the server was never
// touched", which is the correct outcome on most nights. A step returning it
// ends the job in StateSkipped with the remaining steps recorded as skipped,
// so the phase list still reads as a phase list.
var ErrSkipJob = errors.New("jobs: nothing to do")

// ErrNoRunner is a job kind this build cannot execute.
//
// Built on avail.New so it matches avail.ErrUnavailable: a scheduled kind this
// build has no runner for exits 3, the same code `gvm install` and `gvm update`
// give, so `gvm job run` under systemd and an operator at a prompt get the same
// answer. errors.Is(err, ErrNoRunner) still works for the narrower match.
var ErrNoRunner = avail.New("jobs: no runner for this job kind")

// ErrEnv is an environment a job cannot run in.
var ErrEnv = errors.New("jobs: the job environment is incomplete")

// Env is everything one job run needs that is not the work itself.
//
// Every host-touching thing in it is a seam with a fake, which is rule 5 of
// this project rather than a testing convenience: TestNeverLeavesServerStopped
// has to run on Windows, against a fake systemd and a fake provider, or the
// one invariant that matters is only ever checked on the box it is already too
// late to check it on.
type Env struct {
	// Identity, as resolved from the slug (D-13).
	Instance string
	Job      string
	Slug     string
	Action   string
	Kind     lifecycle.JobKind

	// Unit is the job's own systemd unit, recorded in the lease and the
	// ledger so that `journalctl -u <unit>` is in the row.
	Unit      string
	Trigger   Trigger
	Actor     audit.Actor
	RequestID string

	// JobID is the job_runs row id when Postgres answered. Zero is normal:
	// the trigger is systemd and the ledger is Postgres, never the reverse
	// (D-29), so a job runs and records its lease with the database down.
	JobID int64

	// ManagedInstall is spec.ManagedInstall: false is the adoption flag, and
	// it is here rather than looked up because the update pipeline's decision
	// (phase 2) turns on it and that decision is pure.
	ManagedInstall bool

	// Super is how the job sees and changes whether the server is running.
	// Required for every instance-scoped kind, including the ones that never
	// stop anything -- the guard needs it to observe.
	Super Supervisor
	// Host is the privileged file surface: the lease and the degraded marker.
	Host Host
	// Audit receives the job.run span. Required: section 3.3's promise is
	// about the SINK failing, not about there being no sink.
	Audit *audit.Logger
	Log   *slog.Logger
	// Now is the clock. Injectable so a run's durations and the holder line's
	// timestamp are assertable.
	Now func() time.Time

	// PID and BootID go in the lease. BootID is what makes "a lease from
	// before the reboot" decidable instead of guessed at.
	PID    int
	BootID string

	// Locker takes the job lock. Nil is FlockLocker, which is a real file and
	// a real flock(2).
	Locker Locker
	// Flock is the lock primitive FlockLocker uses. Nil is the platform's.
	Flock Flock

	// StopBudget bounds the shutdown ladder a step runs. Zero lets the
	// instance's own shutdown.budget decide, which is what the rendered
	// ladder already carries.
	StopBudget time.Duration
	// RestoreBudget bounds the guard's restore. Zero is
	// DefaultRestoreBudget.
	RestoreBudget time.Duration
	// LeaseFor is the lease's self-expiry backstop. Zero writes no deadline,
	// which is correct: lifecycle.Lease.Classify only consults the deadline
	// when liveness cannot be established at all.
	LeaseFor time.Duration

	// WaitReady, when set, is how the guard and the start steps wait for the
	// game rather than for the unit. Optional, and bounded by its own
	// implementation.
	WaitReady func(ctx context.Context, name string) error

	// Policy overrides the kind's default (spec.Job.OnFailure). Empty uses
	// the kind's.
	Policy Policy

	// Confirm is section 16.4's typed confirmation, required for every kind
	// jobs.Destructive reports true for and ignored for the rest.
	//
	// It is checked in validate, which runs before the lock and before the
	// guard, so a destructive kind cannot be reached without it by any path
	// -- the CLI, the API and the dashboard all arrive through Execute, and a
	// check in any one of them would be a check the other two could forget.
	Confirm *Confirmation

	// The work seams. Each kind needs exactly one and its Steps builder says
	// so by name, so a job wired without its engine fails before it has
	// stopped anything rather than halfway through.
	Backups  Backups
	Updates  Updates
	Contents Contents
	Archives Archives
	Rebuilds Rebuilds
	Sweeper  Sweeper
}

func (e *Env) logger() *slog.Logger {
	if e != nil && e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

func (e *Env) clock() time.Time {
	if e != nil && e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Env) restoreBudget() time.Duration {
	if e != nil && e.RestoreBudget > 0 {
		return e.RestoreBudget
	}
	return DefaultRestoreBudget
}

// unwindBudget bounds the step Undos. It follows RestoreBudget when a test
// overrode it, so a rig that shortens the deferred chain shortens all of it.
func (e *Env) unwindBudget() time.Duration {
	if e != nil && e.RestoreBudget > 0 {
		return e.RestoreBudget
	}
	return UnwindBudget
}

// Step is one piece of a job's work.
type Step struct {
	// Name is the operation, and it is the same word the seam's method is
	// named after. That is what lets the never-leaves-stopped test enumerate
	// its injection points FROM THE STEP TABLES rather than from a
	// hand-written list of forty -- so a new phase is covered the moment it
	// is added, and a phase whose failure nobody injected cannot exist.
	Name string
	// Do is the work. A nil Do is a programming error, not a no-op.
	Do func(ctx context.Context, jr *Run) error
	// Undo is this step's own defer: run while the job unwinds, in reverse
	// order, for every step whose Do STARTED -- failure included.
	//
	// It is the same discipline as the running guard, one level down, and it
	// exists so that a freeze is thawed and a masked timer is unmasked
	// without burying those two injection points inside one opaque step. A
	// freeze that is not thawed leaves a running server that has silently
	// stopped persisting, which section 13.2 calls the worst failure in the
	// system.
	Undo func(ctx context.Context, jr *Run) error
	// Fatal says a failure here ends the job.
	//
	// A step that is NOT fatal is update.sh's `|| warn`: the failure is
	// recorded, the run is degraded, and the job carries on -- which is how
	// "Backup failed - continuing anyway" and "Steam update failed - starting
	// on the existing binaries" are expressed here.
	Fatal bool
	// Stops says this step may take the server down.
	//
	// Declared rather than inferred, so the engine can assert the guard was
	// armed before the first one. A step table that stops a server with no
	// guard in front of it is the bug this whole package is about, and it
	// panics rather than runs.
	Stops bool
	// When, when non-nil, decides whether the step runs at all. A step it
	// declines is RECORDED as skipped with its reason, because "phase 5 did
	// not run because the build had not changed" is the answer an operator
	// wants and an absent row is not it.
	When func(jr *Run) (bool, string)
}

// Runner is one job kind: section 13.3's row, as code.
type Runner struct {
	Kind  lifecycle.JobKind
	Scope Scope
	// Policy is the kind's default failure policy, column four.
	Policy Policy
	// Stops is column three: whether this kind may stop the server at all.
	// It is asserted against the step tables, so "guard | starts, never
	// stops" is a property of the code and not of the documentation.
	Stops bool
	// Steps builds the table for one run. It is a function and not a slice
	// because the steps of a run share state -- the archive ref the prune
	// must not run without, the build diff the decision turns on -- and a
	// closure is where that state belongs.
	Steps   func(env *Env) ([]Step, error)
	Summary string
}

// runners is the closed set of kinds this build can execute.
//
// Assembled here rather than by init() in each file: a registry that packages
// append to is a registry whose contents depend on which files were compiled,
// and the one question an operator asks of `gvm job run` is "will this kind
// run". It is answered by reading one table.
var runners = map[lifecycle.JobKind]*Runner{
	lifecycle.JobBackup:      backupRunner,
	lifecycle.JobUpdate:      updateRunner,
	lifecycle.JobRestart:     restartRunner,
	lifecycle.JobContentSync: contentRunner,
	lifecycle.JobRebuild:     rebuildRunner,
	lifecycle.JobGuard:       guardRunner,
	lifecycle.JobPrune:       pruneRunner,
	lifecycle.JobVerify:      verifyRunner,
}

// pending are the kinds whose runner lands with the package that does the
// work, and the message says which package.
//
// It is a table rather than a TODO comment because the refusal has to be
// honest: `gvm job run pz-main_rebuild` on a build without a rebuild runner
// must say so, and slug_test.go asserts that every action an instance YAML may
// schedule is in exactly one of the two tables -- so the gap cannot be
// forgotten and cannot be duplicated.
var pending = map[lifecycle.JobKind]string{
	lifecycle.JobRestore: "internal/backup's restore path (section 13.2)",
}

// Kinds returns the job kinds this build can execute, in schema order.
func Kinds() []lifecycle.JobKind {
	out := make([]lifecycle.JobKind, 0, len(runners))
	for _, k := range lifecycle.JobKinds() {
		if _, ok := runners[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// Lookup returns the runner for a kind.
func Lookup(k lifecycle.JobKind) (*Runner, error) {
	if r, ok := runners[k]; ok {
		return r, nil
	}
	if where, ok := pending[k]; ok {
		return nil, fmt.Errorf("%w: %q is implemented by %s, which this build does not have",
			ErrNoRunner, k, where)
	}
	return nil, fmt.Errorf("%w: %q (this build runs %s)", ErrNoRunner, k,
		strings.Join(kindStrings(), ", "))
}

func kindStrings() []string {
	ks := Kinds()
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, string(k))
	}
	return out
}

// Run is one job execution: the `job_runs` row, assembled on disk.
//
// It is a value and not a database handle for D-29's reason. The trigger is
// systemd and the ledger is Postgres, never the reverse -- so the job produces
// this, logs it, and hands it to whatever is able to record it. A failed
// insert logs a warning and does not stop the job.
type Run struct {
	Slug     string            `json:"slug"`
	Instance string            `json:"instance_name"`
	Job      string            `json:"job_name,omitempty"`
	Kind     lifecycle.JobKind `json:"kind"`
	Action   string            `json:"action,omitempty"`
	Unit     string            `json:"unit,omitempty"`
	Trigger  Trigger           `json:"trigger"`
	State    State             `json:"state"`

	// RunAs is `job_runs.run_as`, recorded so that a future regression --
	// third-party code running as root rather than as the account that owns
	// the destination (D-16) -- is visible in history.
	RunAs  string `json:"run_as"`
	PID    int    `json:"pid,omitempty"`
	BootID string `json:"boot_id,omitempty"`

	StartedAt time.Time     `json:"started_at"`
	EndedAt   time.Time     `json:"finished_at,omitzero"`
	Took      time.Duration `json:"duration,omitzero"`

	Steps []StepResult `json:"steps,omitempty"`
	// Warnings are the non-fatal failures: update.sh's `|| warn` lines.
	Warnings []string `json:"warnings,omitempty"`
	// Notes are the sentences a step wanted an operator to read.
	Notes []string `json:"notes,omitempty"`
	// Summary is `job_runs.summary`: phases, bytes, stall_ms, builds.
	Summary map[string]any `json:"summary,omitempty"`

	// Guard is what the running guard observed and did.
	Guard Restoration `json:"guard"`
	// LeftStopped is section 14.4's column, and "true here is a doctor
	// error".
	LeftStopped bool `json:"left_stopped"`
	Degraded    bool `json:"degraded,omitempty"`

	SkipReason string `json:"skip_reason,omitempty"`
	// Panic is the recovered panic's value. A panic in a job is recovered,
	// audited and reported rather than taking the process down, because the
	// deferred guard is the thing that must still run.
	Panic string `json:"panic,omitempty"`
	// Fatal is the failure that ended the job.
	Fatal error `json:"-"`
	// FatalErr is Fatal's message, for the serialised copy.
	FatalErr string `json:"error,omitempty"`
}

// Set records a summary field.
func (r *Run) Set(key string, v any) {
	if r.Summary == nil {
		r.Summary = map[string]any{}
	}
	r.Summary[key] = v
}

// Get reads a summary field back, which is how one step sees what an earlier
// one learned without the two sharing a type.
func (r *Run) Get(key string) (any, bool) {
	v, ok := r.Summary[key]
	return v, ok
}

// Note records a sentence for the operator.
func (r *Run) Note(format string, args ...any) {
	r.Notes = append(r.Notes, sanitise(fmt.Sprintf(format, args...)))
}

// Warn records a non-fatal failure.
func (r *Run) Warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, sanitise(fmt.Sprintf(format, args...)))
}

// Skipped reports whether the run ended because there was nothing to do.
func (r *Run) Skipped() bool { return r.State == StateSkipped }

// Err is the run's failure, if it had one.
//
// The panic is reported before the fatal error because a panic is a gvm defect
// and a fatal error is usually the host's: an operator reading one line wants
// to know which of the two they are looking at.
//
// LeftStopped is last and is not redundant. A job can leave a server stopped
// with no step having failed at all: the entry observation was inconclusive,
// the guard treated the instance as running, and its start did not work. That
// run has no Fatal, and without this case Execute would return a nil error for
// the one outcome section 14.4 calls a doctor error -- so `gvm job run` would
// exit 0 and the timer would report success.
func (r *Run) Err() error {
	switch {
	case r.Panic != "":
		return fmt.Errorf("jobs: %s panicked: %s", r.Slug, r.Panic)
	case r.Fatal != nil:
		return r.Fatal
	case r.LeftStopped:
		return fmt.Errorf("jobs: %s left %s stopped: %s", r.Slug, r.Instance,
			strings.Join(r.Guard.Reasons, "; "))
	}
	return nil
}

// AuditParams is the job.run row's params, against section 14.8's allowlist
// (kind, unit, run_as, left_stopped; kind required).
func (r *Run) AuditParams() audit.Params {
	p := audit.Params{"kind": string(r.Kind), "left_stopped": r.LeftStopped}
	if r.Unit != "" {
		p["unit"] = r.Unit
	}
	if r.RunAs != "" {
		p["run_as"] = r.RunAs
	}
	return p
}

// Line is the one line a CLI prints when a job ends.
func (r *Run) Line() string {
	var b strings.Builder
	b.WriteString(string(r.State))
	b.WriteString(" ")
	b.WriteString(r.Slug)
	b.WriteString(" (")
	b.WriteString(string(r.Kind))
	b.WriteString(")")
	if r.Took > 0 {
		b.WriteString(" in ")
		b.WriteString(r.Took.Round(time.Millisecond).String())
	}
	switch {
	case r.LeftStopped:
		b.WriteString(" -- THE SERVER IS STOPPED")
	case r.Guard.Started:
		b.WriteString(" -- the guard restarted it")
	case r.SkipReason != "":
		b.WriteString(": ")
		b.WriteString(r.SkipReason)
	}
	if n := len(r.Warnings); n > 0 {
		b.WriteString(fmt.Sprintf("; %d warning(s)", n))
	}
	return b.String()
}

// Report is the multi-line account: every phase, every warning, the guard.
func (r *Run) Report() string {
	var b strings.Builder
	b.WriteString(r.Line())
	b.WriteString("\n")
	for _, s := range r.Steps {
		switch {
		case s.Skipped:
			b.WriteString("  - " + s.Name + " skipped")
			if s.Why != "" {
				b.WriteString(": " + s.Why)
			}
		case s.Failed():
			mark := "warn"
			if s.Fatal {
				mark = "FAIL"
			}
			b.WriteString("  " + mark + " " + s.Name + ": " + s.Err)
		default:
			b.WriteString("  ok   " + s.Name)
		}
		if s.UndoErr != "" {
			b.WriteString("\n       cleanup failed: " + s.UndoErr)
		}
		b.WriteString("\n")
	}
	for _, n := range r.Notes {
		b.WriteString("  note " + n + "\n")
	}
	for _, g := range r.Guard.Reasons {
		b.WriteString("  guard " + g + "\n")
	}
	if r.Guard.Note != "" {
		b.WriteString("  note " + r.Guard.Note + "\n")
	}
	return b.String()
}

// Execute resolves the slug's kind to a runner and runs it.
func Execute(ctx context.Context, env *Env) (*Run, error) {
	r, err := Lookup(env.Kind)
	if err != nil {
		return nil, err
	}
	return r.Execute(ctx, env)
}

// validate refuses an environment a job cannot be run honestly in.
//
// Every check here fails BEFORE anything has been stopped, which is why they
// are checks and not defaults: a job that defaulted its way past a missing
// supervisor would discover the gap after the shutdown ladder.
func (r *Runner) validate(env *Env) error {
	if env == nil {
		return fmt.Errorf("%w: no environment", ErrEnv)
	}
	if env.Kind != r.Kind {
		return fmt.Errorf("%w: %s runner given a %q environment", ErrEnv, r.Kind, env.Kind)
	}
	if !r.Scope.Accepts(env.Instance) {
		if env.Instance == "" {
			return fmt.Errorf("%w: %s is an %s job and the slug names no instance (D-13)",
				ErrEnv, r.Kind, r.Scope)
		}
		return fmt.Errorf("%w: %s is a %s job and the slug names the instance %q",
			ErrEnv, r.Kind, r.Scope, env.Instance)
	}
	if env.Instance != "" {
		if err := instance.ValidateName(env.Instance); err != nil {
			return err
		}
		if env.Super == nil {
			return fmt.Errorf("%w: %s needs a supervisor: the running guard cannot observe or "+
				"restore %s without one", ErrEnv, r.Kind, env.Instance)
		}
		if env.Host == nil {
			// Required, not optional. Without it the lease is never taken --
			// so `gvm start` would not refuse to race this job -- and the
			// degraded breadcrumb is never written, so `gvm-guard.timer` has
			// nothing to act on if the guard's own start fails. Both are
			// silent absences, which is why this is a refusal before the job
			// begins rather than a warning inside it.
			return fmt.Errorf("%w: %s needs a privileged surface: without one it can take no "+
				"lease on %s and leave no breadcrumb if it cannot restore it",
				ErrEnv, r.Kind, env.Instance)
		}
	}
	// Section 16.4, before anything: a destructive kind that nobody typed the
	// instance name for does not reach the lock, let alone the destroy. It is
	// placed above the audit check deliberately -- a refused confirmation is
	// not a job run, and a run row for one would make `gvm job list` read as
	// though somebody had attempted a rebuild that the engine in fact never
	// started.
	if Destructive(r.Kind) {
		if err := env.Confirm.Check(env.Instance, r.Kind, env.Trigger); err != nil {
			return err
		}
	}
	if env.Audit == nil {
		return fmt.Errorf("%w: %s has nowhere to record; every job run is auditable",
			ErrEnv, r.Kind)
	}
	if env.Trigger != "" && !env.Trigger.Valid() {
		return fmt.Errorf("%w: %q is not a job trigger", ErrEnv, env.Trigger)
	}
	if env.Policy != "" && !env.Policy.Valid() {
		return fmt.Errorf("%w: %q is not a failure policy", ErrEnv, env.Policy)
	}
	return nil
}

func (r *Runner) policy(env *Env) Policy {
	if env.Policy != "" {
		return env.Policy
	}
	if r.Policy != "" {
		return r.Policy
	}
	return PolicyFail
}

// Execute runs one job, and the ordering of its defers is the design.
//
// It returns the Run even on failure. Every caller -- the CLI, gvmd, the guard
// timer -- needs the record more than it needs the error, because the record
// is what says whether the server is up.
func (r *Runner) Execute(ctx context.Context, env *Env) (jr *Run, err error) {
	if verr := r.validate(env); verr != nil {
		return nil, verr
	}
	now := env.clock()
	jr = &Run{
		Slug: env.Slug, Instance: env.Instance, Job: env.Job, Kind: r.Kind,
		Action: env.Action, Unit: env.Unit, Trigger: env.Trigger,
		State: StateRunning, RunAs: "root", PID: env.PID, BootID: env.BootID,
		StartedAt: now,
	}
	if jr.Slug == "" {
		jr.Slug = Slug{Instance: env.Instance, Job: env.Job}.String()
	}

	// 1. The lock. Before the audit span, because a job that could not even
	//    take the lock has not started and should not leave a `started` row
	//    whose terminal row says "somebody else was running".
	lock, lerr := r.acquire(env, jr)
	if lerr != nil {
		jr.State = StateFailed
		jr.Fatal = lerr
		jr.FatalErr = lerr.Error()
		jr.EndedAt = env.clock()
		jr.Took = jr.EndedAt.Sub(jr.StartedAt)
		env.logger().Warn("a job did not run", "slug", jr.Slug, "err", lerr)
		return jr, lerr
	}
	defer func() { _ = lock.Release() }()

	span := env.Audit.Start(ctx, audit.Event{
		Actor: env.Actor, Action: audit.JobRun, Instance: env.Instance,
		Target: jr.Slug, RequestID: env.RequestID,
		Params: audit.Params{"kind": string(r.Kind)},
	})

	// 2. THE RUNNING GUARD. This line is why the file is shaped like this.
	//    Nothing fallible has happened to the instance yet and nothing will
	//    until the guard is armed.
	g, gerr := Guard(ctx, env)
	if gerr != nil {
		jr.State = StateFailed
		jr.Fatal = gerr
		jr.FatalErr = gerr.Error()
		jr.EndedAt = env.clock()
		jr.Took = jr.EndedAt.Sub(jr.StartedAt)
		span.Done(ctx, gerr, jr.AuditParams())
		return jr, gerr
	}

	// Registered FIRST so it runs LAST: it reads what the guard did.
	defer func() {
		if p := recover(); p != nil {
			jr.Panic = sanitise(fmt.Sprint(p))
		}
		r.finish(ctx, env, jr, g, span)
		if e := jr.Err(); e != nil {
			err = e
		}
	}()
	// Registered second so it runs second-to-last: the guard's restore.
	defer g.Restore(ctx)
	// Registered third so it runs before the restore. That order is not
	// cosmetic: lifecycle.Allowed refuses a start while a LIVE lease holds the
	// instance, and the live lease at that moment would be this job's own.
	defer r.clearLease(ctx, env, jr)

	// 3. The lease. Fatal, and safely so: nothing has stopped yet, and a job
	//    running without one is a job `gvm start` will not refuse to race.
	if err := r.writeLease(ctx, env, jr); err != nil {
		jr.Fatal = err
		jr.FatalErr = err.Error()
		return jr, err
	}

	// 4. The steps.
	steps, berr := r.Steps(env)
	if berr != nil {
		jr.Fatal = berr
		jr.FatalErr = berr.Error()
		return jr, berr
	}
	started := make([]*Step, 0, len(steps))
	// Registered last so it runs FIRST: thaw the freeze and unmask the timers
	// before the lease is released and the server is restarted.
	defer func() { r.unwind(ctx, env, jr, started) }()

	r.steps(ctx, env, jr, g, steps, &started)
	return jr, jr.Err()
}

// steps runs the table.
//
// The two things it does that a plain loop would not: it records a declined
// step rather than omitting it, and it panics on a step that may stop the
// server with no guard armed. The panic is deliberate and is archive.Destroy's
// reasoning applied here -- reaching that state is a programming error, a
// panic in a job is recovered and audited, and a silent skip could be routed
// around by a future caller.
func (r *Runner) steps(ctx context.Context, env *Env, jr *Run, g *RunningGuard,
	steps []Step, started *[]*Step) {

	for i := range steps {
		s := &steps[i]
		if s.Do == nil {
			panic(fmt.Sprintf("jobs: %s step %q has no work", r.Kind, s.Name))
		}
		if s.Stops && !g.Armed() {
			panic(fmt.Sprintf("jobs: %s step %q may stop the server and no running guard is "+
				"armed; the guard is installed before any fallible work, not after the first "+
				"step that might fail (section 13.4)", r.Kind, s.Name))
		}
		if s.Stops && !r.Stops {
			panic(fmt.Sprintf("jobs: %s declares that it never stops the server and step %q "+
				"does (section 13.3)", r.Kind, s.Name))
		}
		if jr.State == StateSkipped || jr.Fatal != nil {
			jr.Steps = append(jr.Steps, StepResult{
				Name: s.Name, Skipped: true, Why: "the job ended before this phase",
			})
			continue
		}
		if s.When != nil {
			if ok, why := s.When(jr); !ok {
				if why == "" {
					why = "not applicable to this run"
				}
				jr.Steps = append(jr.Steps, StepResult{Name: s.Name, Skipped: true, Why: why})
				continue
			}
		}
		// A cancelled context is not a step failure: it is the job being told
		// to stop. Recorded, and then the unwind and the guard do their work
		// on a context of their own.
		if cerr := ctx.Err(); cerr != nil {
			jr.Steps = append(jr.Steps, StepResult{
				Name: s.Name, Skipped: true,
				Why: "the job was cancelled before this phase: " + cerr.Error(),
			})
			if jr.State != StateCancelled {
				jr.State = StateCancelled
				jr.Note("cancelled during %s: %v", s.Name, cerr)
			}
			continue
		}

		*started = append(*started, s)
		at := env.clock()
		serr := s.Do(ctx, jr)
		res := StepResult{Name: s.Name, Started: at, Took: env.clock().Sub(at)}

		switch {
		case serr == nil:
		case errors.Is(serr, ErrSkipJob):
			jr.State = StateSkipped
			res.Skipped = true
			res.Why = skipReason(serr)
			jr.SkipReason = res.Why
		case s.Fatal:
			res.Err = serr.Error()
			res.Fatal = true
			jr.Fatal = fmt.Errorf("%s: %w", s.Name, serr)
			jr.FatalErr = jr.Fatal.Error()
		default:
			// update.sh's `|| warn`, as a field rather than as a habit.
			res.Err = serr.Error()
			jr.Warn("%s: %v", s.Name, serr)
			env.logger().Warn("a job step failed and the job continued",
				"slug", jr.Slug, "step", s.Name, "err", serr)
		}
		jr.Steps = append(jr.Steps, res)
	}
}

// skipReason recovers the sentence a step wrapped around ErrSkipJob.
func skipReason(err error) string {
	s := err.Error()
	if s == ErrSkipJob.Error() {
		return "nothing to do"
	}
	return strings.TrimSuffix(strings.TrimSuffix(s, ErrSkipJob.Error()), ": ")
}

// unwind runs the Undo of every step that started, in reverse order.
//
// On a context of its own, for the guard's reason: the undo list is where a
// freeze is thawed, and a cancelled job whose freeze was never thawed leaves a
// running server that has silently stopped saving.
func (r *Runner) unwind(ctx context.Context, env *Env, jr *Run, started []*Step) {
	// UnwindBudget and not the restore's: the two used to share one number
	// and the four deferred legs then added up to more than the unit's
	// TimeoutStopSec, so SIGKILL could land inside the guard's own start.
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), env.unwindBudget())
	defer cancel()

	for i := len(started) - 1; i >= 0; i-- {
		s := started[i]
		if s.Undo == nil {
			continue
		}
		err := func() (err error) {
			// One step's broken cleanup must not skip the next one's, and must
			// not replace a panic already in flight.
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("panicked: %v", p)
				}
			}()
			return s.Undo(uctx, jr)
		}()
		if err == nil {
			continue
		}
		jr.Degraded = true
		for j := range jr.Steps {
			if jr.Steps[j].Name == s.Name {
				jr.Steps[j].UndoErr = err.Error()
			}
		}
		jr.Warn("%s cleanup: %v", s.Name, err)
		env.logger().Error("a job step's cleanup failed",
			"slug", jr.Slug, "step", s.Name, "err", err)
	}
}

// finish computes the verdict and closes the audit span.
func (r *Runner) finish(ctx context.Context, env *Env, jr *Run, g *RunningGuard, span *audit.Span) {
	jr.Guard = g.Result()
	if jr.Guard.LeftStopped {
		jr.LeftStopped = true
	}
	if jr.Guard.Degraded {
		jr.Degraded = true
	}
	jr.EndedAt = env.clock()
	jr.Took = jr.EndedAt.Sub(jr.StartedAt)
	jr.State = r.Verdict(env, jr)

	// A context of its own, so that a cancelled job still records its own
	// outcome. The terminal audit row is the evidence that the guard ran.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), FinishBudget)
	defer cancel()
	span.Done(actx, jr.Err(), jr.AuditParams())

	lg := env.logger().With("slug", jr.Slug, "kind", string(jr.Kind),
		"state", string(jr.State), "took", jr.Took, "left_stopped", jr.LeftStopped)
	switch {
	case jr.LeftStopped:
		lg.Error("a job left a server stopped; gvm-guard.timer runs within 5 minutes")
	case jr.State.Bad():
		lg.Warn("a job finished badly")
	default:
		lg.Info("a job finished")
	}
}

// Verdict is the state a finished run records, and it is pure.
//
// The order is the priority order, and the first case is the one that matters:
// a server left stopped is `failed` whatever the kind's policy says, because
// that is the outcome section 14.4 calls a doctor error and no failure policy
// may downgrade it.
func (r *Runner) Verdict(env *Env, jr *Run) State {
	switch {
	case jr.LeftStopped:
		return StateFailed
	case jr.Panic != "":
		return StateFailed
	case jr.Fatal != nil && len(jr.Steps) == 0:
		// The job never ran a phase: a missing work engine, a lease it could
		// not take, a step table that would not build. No failure policy
		// downgrades this, because `degraded` means the work happened and
		// something was wrong, and here the work did not happen at all.
		return StateFailed
	case jr.Fatal != nil:
		switch r.policy(env) {
		case PolicyFail:
			// backup: a gap in history is loud.
			return StateFailed
		case PolicyWarn:
			// prune, verify, guard: nothing was stopped and nothing could be,
			// so the cost is disk or evidence and never availability.
			return StateDegraded
		default:
			// start_anyway: degraded while the server is serving, failed
			// otherwise. The second half is not redundant with LeftStopped --
			// a job on an instance that was already down when it started can
			// fail without the guard having anything to restore.
			if jr.Guard.Armed && !jr.Guard.ActiveAfter {
				return StateFailed
			}
			return StateDegraded
		}
	case jr.State == StateCancelled:
		return StateCancelled
	case jr.State == StateSkipped:
		return StateSkipped
	case len(jr.Warnings) > 0 || jr.Degraded:
		return StateDegraded
	}
	return StateOK
}

// acquire takes the job lock, naming the holder (D-30).
//
// The holder line is pid:unit:kind:started_at and all four fields are filled
// here, because this is the only place that knows all four: a lock that said
// only "held" is useless at 2am, and one that names the pid and when it was
// taken tells the operator whether to wait or to clear it.
func (r *Runner) acquire(env *Env, jr *Run) (Unlocker, error) {
	s := Slug{Instance: env.Instance, Job: env.Job}
	lk := env.Locker
	if lk == nil {
		lk = FlockLocker{LockOptions{Flock: env.Flock, Now: env.Now}}
	}
	return lk.Acquire(s.LockPath(), lifecycle.Holder{
		PID:       env.PID,
		Unit:      env.Unit,
		Kind:      string(r.Kind),
		StartedAt: jr.StartedAt,
	})
}

// writeLease records which job owns this instance (section 5.3).
//
// Through hostops, like the hold and the degraded marker: taking it is what
// makes `gvm start` refuse, so an unaudited lease is an unaudited refusal.
func (r *Runner) writeLease(ctx context.Context, env *Env, jr *Run) error {
	if env.Instance == "" || env.Host == nil {
		return nil
	}
	l := lifecycle.Lease{
		Instance: env.Instance, Kind: r.Kind, JobID: env.JobID, Unit: env.Unit,
		PID: env.PID, BootID: env.BootID, StartedAt: jr.StartedAt,
		Reason: r.leaseReason(env),
	}
	if env.LeaseFor > 0 {
		l.Deadline = jr.StartedAt.Add(env.LeaseFor)
	}
	body, err := l.Bytes()
	if err != nil {
		return fmt.Errorf("jobs: render the lease: %w", err)
	}
	if _, err := env.Host.Do(ctx, hostops.Request{
		Verb: hostops.VerbFileWrite, Actor: env.Actor, Instance: env.Instance,
		File: hostops.FileLease, Body: body, RequestID: env.RequestID,
	}); err != nil {
		return fmt.Errorf("jobs: take the lease on %s: %w", env.Instance, err)
	}
	return nil
}

func (r *Runner) leaseReason(env *Env) string {
	if env.Job != "" && env.Job != string(r.Kind) {
		return string(r.Kind) + " (job " + env.Job + ")"
	}
	return string(r.Kind)
}

// clearLease releases the instance.
//
// A failure here is recorded and never fatal, and it deliberately does not
// stop the guard: a lease gvm could not remove is a stale lease, which
// lifecycle classifies and `gvm job clear` resolves -- whereas a server left
// stopped has no such path.
func (r *Runner) clearLease(ctx context.Context, env *Env, jr *Run) {
	if env.Instance == "" || env.Host == nil {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ClearLeaseBudget)
	defer cancel()
	if _, err := env.Host.Do(cctx, hostops.Request{
		Verb: hostops.VerbFileRemove, Actor: env.Actor, Instance: env.Instance,
		File: hostops.FileLease, RequestID: env.RequestID,
	}); err != nil {
		jr.Degraded = true
		jr.Warn("could not release the lease on %s: %v", env.Instance, err)
		env.logger().Error("a job could not release its lease",
			"instance", env.Instance, "slug", jr.Slug, "err", err,
			"recovery", "gvm job clear "+env.Instance)
	}
}

// StepNames is every step a kind would run, in order, without running any.
//
// It exists for two callers. `gvm update <n> --diff` prints the phases it
// would execute, and TestNeverLeavesServerStopped enumerates its injection
// points from it -- which is what makes a newly added phase covered by that
// test the moment it is added rather than the next time somebody remembers to
// extend a list of forty.
func StepNames(env *Env) ([]string, error) {
	r, err := Lookup(env.Kind)
	if err != nil {
		return nil, err
	}
	steps, err := r.Steps(env)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Name)
	}
	return out, nil
}

// StoppingSteps is the subset that may take the server down.
func StoppingSteps(env *Env) ([]string, error) {
	r, err := Lookup(env.Kind)
	if err != nil {
		return nil, err
	}
	steps, err := r.Steps(env)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range steps {
		if s.Stops {
			out = append(out, s.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}
