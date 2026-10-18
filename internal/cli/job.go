// `gvm job` — the verb systemd invokes, and the one place a job is assembled.
//
// `gvm-job@.service` runs `gvm job run %i` and nothing else, so
// this file is the whole of what a nightly is: resolve the slug into an
// instance, a job name and a kind, assemble the environment, hand it to
// internal/jobs and print what came back. Every operator verb that drives a
// job -- `gvm backup`, `gvm update`, `gvm rebuild`, `gvm restore` -- arrives at
// the same assembly through buildJobEnv, because a second assembly would be a
// second set of answers to "which supervisor, which lock, which audit sink",
// and the one that mattered would be the one nobody tested.
//
// Two decisions are worth stating.
//
// THE TRIGGER DEFAULTS TO `timer` HERE AND TO `cli` EVERYWHERE ELSE. `gvm job
// run` is not a verb an operator types: it is the unit's ExecStart, and the
// unit is started by a timer or by `systemctl start`. Section 14.4 records the
// trigger because the same failure means different things from different ones
// -- a nightly `update` that fails at 05:03 has nobody watching -- so guessing
// `cli` for a unit-invoked run would put the wrong word in every row the
// dashboard sorts by.
//
// NOTHING HERE DECIDES WHETHER A SERVER COMES BACK. The running guard does,
// unconditionally, inside internal/jobs. This file cannot stop or start
// anything: what it hands the job layer is a jobs.LifecycleSupervisor, which is
// internal/lifecycle's own observer and manager, so a job's stop obeys the same
// precondition table and writes the same audit row as `gvm stop`.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/hostops"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

var jobSubs = []struct {
	name    string
	args    string
	summary string
	run     func(context.Context, *Env, []string) error
}{
	{"run", "<instance>_<job>", "run one job; this is what gvm-job@.service invokes", runJobRun},
	{"list", "", "the jobs this host schedules, and what each one would do", runJobList},
	{"clear", "<instance>", "release a lease a dead job left behind", runJobClear},
}

func runJob(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		jobUsage(env.Stderr)
		return usageErr(errors.New("name a subcommand"))
	}
	switch args[0] {
	case "-h", "--help", "help":
		jobUsage(env.Stdout)
		return nil
	}
	for _, s := range jobSubs {
		if s.name == args[0] {
			return s.run(ctx, env, args[1:])
		}
	}
	jobUsage(env.Stderr)
	return usageErr(fmt.Errorf("unknown subcommand %q", args[0]))
}

func jobUsage(w io.Writer) {
	fmt.Fprint(w, "gvm job — run and inspect the scheduled work\n\nUsage:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range jobSubs {
		fmt.Fprintf(tw, "  gvm job %s %s\t%s\n", s.name, s.args, s.summary)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\n  A slug is \"<instance>_<job>\" and an EMPTY instance part is how a\n"+
		"  host-level job is spelt: %s. Neither half may contain '_',\n"+
		"  so there is exactly one reading of exactly one separator.\n",
		strings.Join(hostSlugs(), ", "))
}

func hostSlugs() []string {
	out := make([]string, 0, len(jobs.HostJobs()))
	for _, j := range jobs.HostJobs() {
		out = append(out, "_"+j)
	}
	return out
}

// jobOptions are the flags every job-driving verb shares.
//
// They are one struct because the four operator verbs below are four spellings
// of one operation, and a flag that meant something different under `gvm
// backup` than under `gvm job run` would be a flag an operator has to look up
// twice.
type jobOptions struct {
	Trigger string
	Policy  string
	Reason  string
	Confirm string
	Yes     bool
	JSON    bool
	Remote  string
	// Lease is the lease's self-expiry backstop.
	Lease time.Duration
	// Engine are the flags that tune a work ENGINE rather than the schedule
	// -- `--deep`, `--keep-world`, `--no-pre-archive`. They live on the
	// shared options struct because the engine that honours them is built
	// during assembly, and a second way to reach it would be a second
	// assembly path.
	Engine engineOptions
}

// bind puts the shared flags on one subcommand's flag set.
//
// The scheduling flags — --trigger, --lease-for, --on-failure — are NOT here.
// They are the unit's knobs, not an operator's: a nightly's trigger is "timer"
// because the timer ran it, and an operator typing `gvm backup pz-main` is a
// CLI trigger whatever they pass. So those three live on `gvm job run` alone
// (bindSchedule), hidden even there, and the operator verbs default them
// through trigger()/leaseFor()/policy() without ever showing a flag that only
// invites a wrong value in the audit ledger.
func (o *jobOptions) bind(fs *flag.FlagSet, defTrigger jobs.Trigger) {
	o.Trigger = string(defTrigger)
	fs.StringVar(&o.Reason, "reason", "", "why, recorded in the audit row and the lease")
	fs.BoolVar(&o.JSON, "json", false, "print the run record as JSON")
	fs.StringVar(&o.Remote, "remote", "", "dashboard URL to run this against instead of the local host")
}

// bindSchedule adds the unit-facing scheduling flags. Only `gvm job run` calls
// it — every job unit runs `gvm job run %i` and needs to say which trigger and
// lease the schedule implies — and even there they are hidden from --help
// (flagUsage lists them in the hidden set) so a human is not nudged to record
// a run as something it was not.
func (o *jobOptions) bindSchedule(fs *flag.FlagSet, defTrigger jobs.Trigger) {
	fs.StringVar(&o.Trigger, "trigger", string(defTrigger),
		"what caused this run, as the ledger records it: "+strings.Join(triggerNames(), " | "))
	fs.StringVar(&o.Policy, "on-failure", "",
		"override the kind's failure policy: fail | start_anyway | warn")
	fs.DurationVar(&o.Lease, "lease-for", jobs.DefaultLeaseFor,
		"the lease's self-expiry backstop")
}

// bindConfirm adds section 16.4's flags. Only the destructive verbs call it:
// a `--confirm` on `gvm backup` would suggest there was something to confirm.
func (o *jobOptions) bindConfirm(fs *flag.FlagSet) {
	fs.StringVar(&o.Confirm, "confirm", "",
		"the instance name, typed, as the destructive-action contract requires")
	fs.BoolVar(&o.Yes, "yes", false,
		"automation: assert that a human already decided. Refused over the API, which must "+
			"carry the typed name instead")
}

// timerTriggerAtTerminal reports whether a `--trigger timer` should be refused.
// It is a pure function so the decision is tested without faking a terminal: a
// timer trigger means systemd ran the job, so it is wrong only when a human is
// at a terminal (a TTY) and not under a unit; a script (no TTY) and a real unit
// run (INVOCATION_ID set) are both fine.
func timerTriggerAtTerminal(trigger string, underUnit, isTTY bool) bool {
	return trigger == string(jobs.TriggerTimer) && !underUnit && isTTY
}

func triggerNames() []string {
	out := make([]string, 0, len(jobs.Triggers()))
	for _, t := range jobs.Triggers() {
		out = append(out, string(t))
	}
	return out
}

func (o *jobOptions) trigger() (jobs.Trigger, error) {
	t := jobs.Trigger(strings.TrimSpace(o.Trigger))
	if t == "" {
		return jobs.TriggerCLI, nil
	}
	if !t.Valid() {
		return "", fmt.Errorf("--trigger %q is not one of %s", o.Trigger,
			strings.Join(triggerNames(), ", "))
	}
	return t, nil
}

// leaseFor defaults the lease's self-expiry here rather than relying on the
// flag's default.
//
// The flag's default documents the figure in `--help`; this is what makes it
// true for a caller that did not come through a flag set at all -- a future API
// handler, or a test. A zero LeaseFor writes a lease with no deadline, and
// while lifecycle only consults the deadline when liveness cannot be
// established, "cannot be established" is exactly the state a SIGKILLed job on
// a rebooted box leaves behind.
func (o *jobOptions) leaseFor() time.Duration {
	if o.Lease > 0 {
		return o.Lease
	}
	return jobs.DefaultLeaseFor
}

func (o *jobOptions) policy() (jobs.Policy, error) {
	p := jobs.Policy(strings.TrimSpace(o.Policy))
	if p == "" {
		return "", nil
	}
	if !p.Valid() {
		return "", fmt.Errorf("--on-failure %q is not one of fail, start_anyway, warn", o.Policy)
	}
	return p, nil
}

// actor is who the audit row says did this.
//
// A timer-triggered run is `timer` and not `root_cli`, which is the whole
// reason the trigger is a flag rather than a guess: section 14.8's rows are
// read to answer "was anybody watching when this happened", and a nightly
// recorded as an operator's command answers it wrongly.
func (o *jobOptions) actor(t jobs.Trigger) audit.Actor {
	switch t {
	case jobs.TriggerTimer, jobs.TriggerGuard, jobs.TriggerChained:
		return audit.Timer()
	default:
		return audit.RootCLI()
	}
}

func runJobRun(ctx context.Context, env *Env, args []string) error {
	var opt jobOptions
	fs := flag.NewFlagSet("gvm job run", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm job run <instance>_<job>\n\n"+
			"Resolves the slug through /etc/gvm/jobs/<slug>.json and runs it.\n\n")
		// The scheduling flags are the unit's, not the operator's: hide them.
		flagUsage(env.Stderr, fs, "trigger", "lease-for", "on-failure")
	}
	// The default trigger is derived from how the run was invoked: systemd sets
	// INVOCATION_ID for every unit, so a run under a unit is a timer run and an
	// interactive run is a CLI run. This is what makes the audit ledger answer
	// "was anybody watching" correctly without the operator setting a flag.
	underUnit := env.Getenv("INVOCATION_ID") != ""
	defTrigger := jobs.TriggerCLI
	if underUnit {
		defTrigger = jobs.TriggerTimer
	}
	opt.bind(fs, defTrigger)
	opt.bindSchedule(fs, defTrigger)
	opt.bindConfirm(fs)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	// A human at a terminal who types --trigger timer would write a row that
	// claims systemd ran this. Refuse it: the run would succeed but lie to the
	// one question the ledger exists to answer. A script (no TTY) may still pass
	// it, and a real unit run is exactly when it is right.
	if timerTriggerAtTerminal(opt.Trigger, underUnit, stdoutIsTerminal(env)) {
		return usageErr(errors.New("--trigger timer says systemd ran this job; at a terminal, " +
			"drop the flag — the run records as cli"))
	}
	if len(pos) != 1 {
		fs.Usage()
		return fmt.Errorf("name one job slug, not %d", len(pos))
	}
	return runOneJob(ctx, env, "job", opt, func(h *HostEnv) (jobs.Resolved, error) {
		return jobs.Resolve(h.descFS(), pos[0])
	})
}

// runOneJob is the shared body of `gvm job run` and of the four operator
// verbs.
//
// It takes the resolution as a function rather than as a value because the two
// routes answer "what is this job" differently and both answers are correct:
// `gvm job run` reads the descriptor that `gvm apply` wrote, which is the only
// thing that knows a job called "nightly" is an update; `gvm backup pz-main`
// knows its own kind and must work on a box where `gvm apply` has never run.
func runOneJob(ctx context.Context, env *Env, verb string, opt jobOptions,
	resolve func(*HostEnv) (jobs.Resolved, error)) error {

	t, err := Resolve(env, verb, opt.Remote)
	if err != nil {
		return err
	}
	if err := t.RequireLocal(); err != nil {
		return err
	}
	h, err := env.host(ctx)
	if err != nil {
		return err
	}
	res, err := resolve(h)
	if err != nil {
		return err
	}
	jenv, err := buildJobEnv(ctx, env, h, res, opt)
	if err != nil {
		return err
	}
	jr, runErr := jobs.Execute(ctx, jenv)
	if jr == nil {
		// A refusal that never became a run: a missing runner, a confirmation
		// nobody typed, an environment that could not be assembled. There is
		// nothing to print but the reason.
		return runErr
	}
	if opt.JSON {
		if err := writeJSON(env.Stdout, jr); err != nil {
			return err
		}
	} else {
		fmt.Fprint(env.Stdout, "\n"+jr.Report())
		printJobTail(env.Stdout, jr)
	}

	// `gvm job run` inside a systemd unit keeps a UNIT contract, not the
	// operator exit-code table (rule 6). The guard and every gvm-job@ unit run
	// `gvm job run %i` as a oneshot: under the table a routine skip (which would
	// be "nothing to report") or a degraded pass would leave the oneshot FAILED
	// and could fire OnFailure=. So when INVOCATION_ID is set, ok/skipped/
	// degraded/cancelled exit 0 and only a genuine failure exits 1. An unwired
	// kind never produced a jr and its avail error still exits 3 through Main.
	if verb == "job" && env.Getenv("INVOCATION_ID") != "" {
		if jr.State == jobs.StateFailed {
			if runErr == nil {
				runErr = fmt.Errorf("the job failed")
			}
			return runErr
		}
		return nil
	}
	return runErr
}

// printJobTail is the part of a run an operator acts on, after the phase list.
//
// The summary keys are printed in a fixed order rather than as a map dump: a
// nightly's `stall_ms` and an update's `update_available` are the two fields
// somebody actually reads, and burying them in whatever order Go's map
// iteration produced is how they get skimmed past.
func printJobTail(w io.Writer, jr *jobs.Run) {
	for _, k := range []string{
		"decision", "update_available", "build_local", "build_remote",
		"archive_path", "archive_bytes", "stall_ms", "settle_waited_ms",
		"pruned", "verified_shallow", "verified_deep", "recovered", "unrecovered",
		"world_touched", "restore",
	} {
		if v, ok := jr.Get(k); ok {
			fmt.Fprintf(w, "  %-18s %v\n", k, v)
		}
	}
	if jr.LeftStopped {
		fmt.Fprintf(w, "\n  %s IS STOPPED. gvm-guard.timer runs within 5 minutes; "+
			"`gvm start %s` is immediate.\n", jr.Instance, jr.Instance)
	}
}

// buildJobEnv assembles one run. Every host-touching thing in it is a seam
// with a production default, so a test drives the whole engine on Windows.
func buildJobEnv(ctx context.Context, env *Env, h *HostEnv, res jobs.Resolved,
	opt jobOptions) (*jobs.Env, error) {

	trig, err := opt.trigger()
	if err != nil {
		return nil, err
	}
	pol, err := opt.policy()
	if err != nil {
		return nil, err
	}
	actor := opt.actor(trig)

	// Validated before anything derives a unit or a lock path from it.
	// instance.JobSlug PANICS on a name it will not accept -- which is
	// correct where it is called from, because a slug reaching it has already
	// been parsed -- and a panic is a stack trace an operator reads as a
	// crash. Here the names were composed by this process from a kind, so a
	// bad one is a gvm defect, and saying which half is wrong is the useful
	// version of it.
	if res.Slug.Instance != "" {
		if err := instance.ValidateName(res.Slug.Instance); err != nil {
			return nil, err
		}
	}
	if err := instance.ValidateJobName(res.Slug.Job); err != nil {
		return nil, fmt.Errorf("%w (the job action is %q; a job NAME may not contain '_' "+
			"because the slug separator is one)", err, res.Action)
	}

	jenv := &jobs.Env{
		Instance: res.Slug.Instance,
		Job:      res.Slug.Job,
		Slug:     res.Slug.String(),
		Action:   res.Action,
		Kind:     res.Kind,
		Unit:     res.Slug.Unit(),
		Trigger:  trig,
		Actor:    actor,

		Host:   h.Ops,
		Audit:  h.audit(),
		Log:    h.Jobs.Log,
		Now:    h.now,
		PID:    h.pid(),
		BootID: h.BootID,
		Locker: h.Jobs.Locker,

		LeaseFor: opt.leaseFor(),
		Policy:   pol,

		Confirm: &jobs.Confirmation{Typed: opt.Confirm, Automated: opt.Yes, ArchiveAck: true},
	}

	// The supervisor exists for every instance-scoped kind, including the
	// ones that never stop anything: the running guard needs it to OBSERVE.
	// Force is set because this job already holds the instance lease, and
	// lifecycle's own precondition table refuses a start while a live lease
	// exists -- which would be this job's own.
	if res.Slug.Instance != "" {
		jenv.Super = h.supervisor(actor, opt.jobReason(res), true)
	}

	// The failure policy comes from the instance's own jobs block when the
	// flag did not override it, which is what makes `on_failure: fail` in a
	// YAML mean anything (spec.Job.OnFailure).
	if res.Slug.Instance != "" {
		in, _, err := loadInstance(h, res.Slug.Instance)
		if err != nil {
			return nil, err
		}
		jenv.ManagedInstall = in.Spec.ManagedInstall
		if jenv.Policy == "" {
			jenv.Policy = policyFromSpec(in, res.Slug.Job)
		}
	}

	if err := h.attachEngines(ctx, jenv, opt.Engine); err != nil {
		return nil, err
	}
	return jenv, nil
}

// jobReason is what the audit row and the lease say this run is for.
func (o *jobOptions) jobReason(res jobs.Resolved) string {
	if strings.TrimSpace(o.Reason) != "" {
		return strings.TrimSpace(o.Reason)
	}
	return string(res.Kind) + " (" + res.Slug.String() + ")"
}

// policyFromSpec reads spec.Job.OnFailure for the job that is running.
//
// By job NAME and not by action, because an instance may schedule the same
// action twice -- a nightly update at 05:00 that may degrade and a Saturday
// one that must not -- and the policy belongs to the entry, not to the verb.
func policyFromSpec(in *spec.Instance, jobName string) jobs.Policy {
	j, ok := in.Spec.Jobs[jobName]
	if !ok {
		return ""
	}
	if p := jobs.Policy(strings.TrimSpace(j.OnFailure)); p.Valid() {
		return p
	}
	return ""
}

// sortedJobNames keeps a listing stable across runs. Map iteration order is
// not a presentation decision.
func sortedJobNames(in *spec.Instance) []string {
	out := make([]string, 0, len(in.Spec.Jobs))
	for name := range in.Spec.Jobs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// supervisor is the production jobs.Supervisor: internal/lifecycle, nothing
// more.
func (h *HostEnv) supervisor(actor audit.Actor, reason string, force bool) jobs.Supervisor {
	if h.Jobs.Super != nil {
		return h.Jobs.Super
	}
	return jobs.LifecycleSupervisor{
		Obs: h.observer(),
		Mgr: &lifecycle.Manager{
			Obs:      h.observer(),
			Host:     h.Ops,
			Shutdown: h.shutdown(),
			Now:      h.Now,
		},
		Actor:  actor,
		Reason: reason,
		Force:  force,
	}
}

func (h *HostEnv) descFS() jobs.DescFS {
	if h.Jobs.Desc != nil {
		return h.Jobs.Desc
	}
	return jobs.OSDescFS
}

func (h *HostEnv) audit() *audit.Logger {
	if h.Audit != nil {
		return h.Audit
	}
	// Never nil: internal/jobs refuses a run with nowhere to record, and
	// section 3.3's promise is about the SINK failing rather than about there
	// being no sink. A journal-only logger is the honest floor.
	return audit.New(audit.MultiSink{})
}

func (h *HostEnv) pid() int {
	if h.Jobs.PID != 0 {
		return h.Jobs.PID
	}
	return os.Getpid()
}

// ----------------------------------------------------------------------
// gvm job list

func runJobList(ctx context.Context, env *Env, args []string) error {
	var only string
	var asJSON bool
	var remote string
	fs := flag.NewFlagSet("gvm job list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm job list [--instance <name>]\n\n")
		fs.PrintDefaults()
	}
	fs.StringVar(&only, "instance", "", "list one instance's jobs")
	fs.BoolVar(&asJSON, "json", false, "print the listing as JSON")
	fs.StringVar(&remote, "remote", "", "dashboard URL to run this against instead of the local host")
	if _, err := parseInterspersed(fs, args); err != nil {
		return flagErr(err)
	}
	t, err := Resolve(env, "job", remote)
	if err != nil {
		return err
	}
	if err := t.RequireLocal(); err != nil {
		return err
	}
	h, err := env.host(ctx)
	if err != nil {
		return err
	}

	rows, err := jobRows(h, only)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(env.Stdout, rows)
	}
	printJobRows(env.Stdout, rows)
	return nil
}

// jobRow is one schedulable job on this host.
type jobRow struct {
	Slug     string `json:"slug"`
	Instance string `json:"instance,omitempty"`
	Job      string `json:"job"`
	Action   string `json:"action"`
	Kind     string `json:"kind"`
	Schedule string `json:"schedule,omitempty"`
	Timer    string `json:"timer,omitempty"`
	Policy   string `json:"on_failure,omitempty"`
	Stops    bool   `json:"stops"`
	// Runnable is whether THIS BUILD can execute the kind, and Why says what
	// is missing when it cannot. A timer that fires nightly against a kind
	// nothing runs is the least observable failure in this subsystem
	// (slug_test.go's own reasoning), so the listing answers it directly.
	Runnable bool   `json:"runnable"`
	Why      string `json:"why,omitempty"`
}

func jobRows(h *HostEnv, only string) ([]jobRow, error) {
	var out []jobRow
	add := func(r jobRow, kind lifecycle.JobKind) {
		r.Kind = string(kind)
		if runner, err := jobs.Lookup(kind); err == nil {
			r.Stops = runner.Stops
			if r.Policy == "" {
				r.Policy = string(runner.Policy)
			}
		}
		// Whether a timer would actually DO anything is engineWired's call, not
		// jobs.Lookup's: the update, content_sync and rebuild kinds all have a
		// runner registered, but their engine (the provider context, the
		// content apply adapter) is not in this build, so a nightly that fires
		// one fails every night. engineWired is the one predicate that knows,
		// and it is the same answer `gvm apply` and the run itself give.
		if ok, why := engineWired(kind, nil, nil); ok {
			r.Runnable = true
		} else {
			r.Why = "cannot run in this build: " + why
		}
		out = append(out, r)
	}

	// The host-level jobs first: they are scheduled by packaging rather than
	// by any instance's YAML, so an operator looking for "what runs on this
	// box" would otherwise not find them at all.
	if only == "" {
		for _, name := range jobs.HostJobs() {
			s := jobs.Slug{Job: name}
			kind, _ := jobs.KindForAction(name)
			add(jobRow{
				Slug: s.String(), Job: name, Action: name,
				Timer: s.Timer(), Schedule: hostJobSchedule(name),
			}, kind)
		}
	}

	names := []string{only}
	if only == "" {
		all, probs, err := lifecycle.List(h.files())
		if err != nil {
			return nil, err
		}
		for _, p := range probs {
			// Reported rather than fatal, for lifecycle.List's own reason: one
			// unreadable spec must not hide the other four instances' jobs.
			out = append(out, jobRow{Why: p.Source + ": " + p.Detail})
		}
		names = all
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		in, _, err := loadInstance(h, name)
		if err != nil {
			out = append(out, jobRow{Instance: name, Why: err.Error()})
			continue
		}
		for _, jobName := range sortedJobNames(in) {
			j := in.Spec.Jobs[jobName]
			s := jobs.Slug{Instance: name, Job: jobName}
			kind, _ := jobs.KindForAction(j.Action)
			add(jobRow{
				Slug: s.String(), Instance: name, Job: jobName, Action: j.Action,
				Schedule: j.Schedule, Timer: s.Timer(), Policy: j.OnFailure,
			}, kind)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// hostJobSchedule is what packaging/systemd ships for each host-level job.
//
// Stated here so `gvm job list` is a complete answer on a box where the units
// are installed, and deliberately as a note rather than read back from
// systemd: the figures are the shipped ones, and an operator who changed them
// in a drop-in is told to look at the timer.
func hostJobSchedule(name string) string {
	switch name {
	case "guard":
		return "OnBootSec=3min, OnUnitActiveSec=5min"
	case "verify":
		return "nightly (see gvm-job@_verify.timer)"
	}
	return ""
}

func printJobRows(w io.Writer, rows []jobRow) {
	fmt.Fprint(w, "\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "SLUG\tKIND\tSTOPS\tON FAILURE\tSCHEDULE\n")
	for _, r := range rows {
		if r.Slug == "" {
			continue
		}
		stops := "no"
		if r.Stops {
			stops = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Slug, r.Kind, stops, r.Policy, r.Schedule)
	}
	_ = tw.Flush()
	for _, r := range rows {
		if r.Why == "" {
			continue
		}
		if r.Slug == "" {
			fmt.Fprintf(w, "  problem  %s\n", r.Why)
			continue
		}
		fmt.Fprintf(w, "  %s cannot run here: %s\n", r.Slug, r.Why)
	}
}

// ----------------------------------------------------------------------
// gvm job clear

func runJobClear(ctx context.Context, env *Env, args []string) error {
	var force bool
	var remote string
	fs := flag.NewFlagSet("gvm job clear", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm job clear <instance>\n\n"+
			"Removes a lease whose holder is provably gone, so `gvm start` stops refusing.\n\n")
		fs.PrintDefaults()
	}
	fs.BoolVar(&force, "force", false,
		"remove a LIVE lease too; the job holding it is still running and will not know")
	fs.StringVar(&remote, "remote", "", "dashboard URL to run this against instead of the local host")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	name, err := oneInstance(pos)
	if err != nil {
		fs.Usage()
		return err
	}
	t, err := Resolve(env, "job", remote)
	if err != nil {
		return err
	}
	if err := t.RequireLocal(); err != nil {
		return err
	}
	h, err := env.host(ctx)
	if err != nil {
		return err
	}

	o, err := h.observer().Observe(ctx, name)
	if err != nil {
		return err
	}
	switch {
	// o.Lease is nil when there is no lease.json at all, which is the normal
	// state and the one an operator reaching for this command hopes for.
	// LeaseState is consulted as well because a lease that exists and
	// classifies as none is a different fact from a file that is not there.
	case o.Lease == nil && !o.LeaseUnreadable:
		fallthrough
	case o.LeaseState == lifecycle.LeaseNone && !o.LeaseUnreadable:
		fmt.Fprintf(env.Stdout, "\nno lease on %s; nothing to clear\n", name)
		return nil
	case o.LeaseState == lifecycle.LeaseLive && !force:
		// A live lease is a running job. Removing it would let `gvm start`
		// race an update that still has the install tree open, which is the
		// collision the lease exists to prevent -- so the refusal names the
		// holder and the flag rather than making the decision.
		return fmt.Errorf("%s holds a LIVE lease: %s\n"+
			"  the job is still running. Wait for it, or stop its unit "+
			"(`systemctl stop %s`), and only then clear the lease. "+
			"`gvm job clear %s --force` removes it anyway",
			name, o.Lease.Describe(h.now()), o.Lease.Unit, name)
	}

	if _, err := h.Ops.Do(ctx, hostops.Request{
		Verb: hostops.VerbFileRemove, Actor: audit.RootCLI(), Instance: name,
		File: hostops.FileLease,
	}); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "\ncleared the lease on %s (%s)\n", name,
		instance.PathsFor(name).Lease)
	return nil
}
