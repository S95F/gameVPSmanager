// `gvm apply` — the only command that changes what a host looks like.
//
// It builds a plan and then runs it, and both halves come from the same
// function: `--diff` is not a description of what apply intends to do, it is
// the plan apply will execute, printed instead. That is why the two can never
// drift, and it is the property `gvm apply --diff` is worth anything for.
//
// Idempotence is the other property, and it is the one that makes this command
// safe to type when you are not sure what state a box is in. A second apply
// over a converged host reports nothing to do -- not "12 steps, all fine", not
// a reload, not a re-enable -- because a plan that always shows work teaches an
// operator to skim past the one apply that mattered.

package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/reconcile"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/internal/version"
)

type applyOptions struct {
	Name   string
	Diff   bool
	Yes    bool
	Remote string
}

func parseApplyArgs(env *Env, verb string, args []string) (applyOptions, error) {
	var opt applyOptions
	var dryRun, dry bool
	fs := flag.NewFlagSet("gvm "+verb, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: gvm %s <instance> [--diff]\n\n"+
			"Reconciles the host with /etc/gvm/instances/<instance>.yaml.\n"+
			"--diff builds the same plan and applies none of it.\n\n", verb)
		// --dry-run and --dry are the project-wide spelling of the same thing,
		// accepted here so muscle memory works; --diff is the documented name.
		flagUsage(env.Stderr, fs, "dry-run", "dry")
	}
	fs.BoolVar(&opt.Diff, "diff", false, "print the plan and change nothing")
	fs.BoolVar(&dryRun, "dry-run", false, "alias for --diff")
	fs.BoolVar(&dry, "dry", false, "alias for --diff")
	fs.BoolVar(&opt.Yes, "yes", false, "answer the confirmation for a plan that removes data")
	fs.StringVar(&opt.Remote, "remote", "", "dashboard URL to run this against instead of the local host")

	pos, err := parseWithInstance(fs, args)
	if err != nil {
		return applyOptions{}, flagErr(err)
	}
	// The project convention is --dry-run; apply's own documented spelling is
	// --diff. They mean the same "build the plan, change nothing", so fold them.
	opt.Diff = opt.Diff || dryRun || dry
	name, err := oneInstance(pos)
	if err != nil {
		fs.Usage()
		return applyOptions{}, err
	}
	opt.Name = name
	return opt, nil
}

// planned is a built plan plus what it was built from, so the printer can name
// the manifest and the caller can execute without loading anything twice.
type planned struct {
	Plan     *reconcile.Plan
	Instance *spec.Instance
	Manifest *spec.Manifest
	// Unprobed records that the write-probe was allowed to be skipped. It is
	// carried so the output can say the sandbox was NOT verified rather than
	// printing a plan that looks checked.
	Unprobed bool
	// UncheckedSchedules records that the OnCalendar expressions were not put
	// past systemd-analyze on this host.
	UncheckedSchedules bool
	// UnrunnableJobs are the instance's timers whose KIND this build cannot
	// run: the drop-in and the timer are installed and armed, and the nightly
	// would fail every time it fired. apply says so plainly rather than letting
	// an operator discover it at 05:03, because the timer being present is not
	// the same as the work happening (U3).
	UnrunnableJobs []unrunnableJob
}

// unrunnableJob is one timer apply will install against a kind with no engine.
type unrunnableJob struct {
	Job  string
	Kind string
	Why  string
}

// buildApply loads the instance and its PINNED manifest and renders the plan.
//
// The pin rather than the catalog, always: reading the catalog here would let
// `apt upgrade gvm` change a live server's argv on the next apply, which is
// the exact failure D-17 exists to prevent.
func buildApply(ctx context.Context, h *HostEnv, t Transport, name string, diffOnly bool) (*planned, error) {
	in, raw, err := loadInstance(h, name)
	if err != nil {
		return nil, err
	}
	m, err := pinnedManifest(h, in)
	if err != nil {
		return nil, err
	}

	// A hand edit that raised the heap var but left spec.resources.memory_max
	// unchanged is an OOM-SIGKILL waiting to happen (game-catalog-config-4,
	// rule 4): the kernel kills the JVM mid-save when native allocation crosses
	// a ceiling the heap already fills. apply refuses rather than write a
	// drop-in that arms it; `gvm set` (a later phase) recomputes the block. The
	// check runs for --diff too, because a plan that cannot be safely applied
	// should not read as one that can.
	if err := instance.CheckResources(m, spec.VarValues(in.Spec.Vars), in.Spec.Resources); err != nil {
		return nil, err
	}
	if err := validateBackupDir(in); err != nil {
		return nil, err
	}
	// An age IDENTITY (private key) in the instance YAML is a hard error: it is
	// the wrong value for spec.backup.remote (which wants the public recipient),
	// and leaving it would embed the key that decrypts the off-box backups
	// inside every one of them (offsite-backup-dr-3).
	if spec.ContainsAgeSecretKey(raw) {
		return nil, fmt.Errorf("%s contains an age identity (AGE-SECRET-KEY-1…), a PRIVATE key. "+
			"Paste the public recipient (age1…), not the identity: the identity would be copied "+
			"into every archive", instance.PathsFor(name).Spec)
	}

	ins := h.inspector()
	p, err := reconcile.BuildPlan(reconcile.PlanInput{
		Instance:  in,
		Manifest:  m,
		SpecBytes: raw,
		Actor:     actorFor(t),
		Host:      h.Ops,
		Inspect:   ins,
		// A --diff must never need systemd-run: it is not an apply, and a
		// development box has no transient units. An APPLY never sets this,
		// so a sandbox that would hide the game's own save directory is still
		// caught before the drop-in is written.
		AllowUnprobed: diffOnly,
		Probe:         h.Probe,
		HasSecret:     func(id string) bool { return h.hasSecret(name, id) },
		Now:           h.now(),
		Version:       version.Version,
	})
	if err != nil {
		return nil, err
	}

	out := &planned{Plan: p, Instance: in, Manifest: m, Unprobed: diffOnly}

	// The systemd grammar check is a separate step from ValidateCalendar,
	// which runs everywhere and only proves the expression is shaped like an
	// OnCalendar. `unchecked` true means the schedules were NOT put past
	// systemd, and the printer says so rather than a tick nobody earned.
	if len(in.Spec.Jobs) > 0 {
		schedules := make(map[string]string, len(in.Spec.Jobs))
		for job, j := range in.Spec.Jobs {
			schedules[job] = j.Schedule
		}
		unchecked, err := reconcile.CheckSchedules(ctx, reconcile.SystemdCalendar{}, schedules)
		if err != nil {
			return nil, err
		}
		out.UncheckedSchedules = unchecked

		// A timer whose kind this build cannot run is installed all the same —
		// apply reconciles the host with the spec, and the spec declares it —
		// but the printer must not let it read as work that will happen.
		for _, job := range sortedJobNames(in) {
			kind, _ := jobs.KindForAction(in.Spec.Jobs[job].Action)
			if ok, why := engineWired(kind, in, m); !ok {
				out.UnrunnableJobs = append(out.UnrunnableJobs,
					unrunnableJob{Job: job, Kind: string(kind), Why: why})
			}
		}
	}
	return out, nil
}

func runApply(ctx context.Context, env *Env, args []string) error {
	return applyOrDiff(ctx, env, "apply", args)
}

func applyOrDiff(ctx context.Context, env *Env, verb string, args []string) error {
	opt, err := parseApplyArgs(env, verb, args)
	if err != nil {
		return err
	}
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

	pl, err := buildApply(ctx, h, t, opt.Name, opt.Diff)
	if err != nil {
		return err
	}
	if opt.Diff {
		printPlan(env.Stdout, pl)
		return nil
	}

	ins := h.inspector()
	ap := &reconcile.Applier{
		Host:    h.Ops,
		Inspect: ins,
		Out:     env.Stdout,
		Confirm: confirmDestructivePlan(env, opt.Name, opt.Yes),
	}

	// Nothing this phase builds produces a destructive plan, so the ref is
	// nil and Apply refuses before the first step if that ever changes
	// without a `gvm rebuild` to supply one. That refusal is the point: it is
	// cheaper to fail here than to discover the ordering was wrong afterwards.
	var ref *archive.Ref

	fmt.Fprintf(env.Stdout, "\napplying %s\n", opt.Name)
	rep, err := ap.Apply(ctx, pl.Plan, ref)
	if rep != nil {
		printApplyReport(env.Stdout, pl, rep)
	}
	return err
}

// confirmDestructivePlan is section 16.4's contract at the CLI edge.
//
// A nil Confirm is a REFUSAL in internal/reconcile, never an assent, so this
// always returns a function -- one that reads the instance name from the
// terminal, exactly as rebuild.sh made you type it. `--yes` pre-answers it,
// which is what a nightly job needs and what an operator at 2am should not be
// using.
func confirmDestructivePlan(env *Env, name string, yes bool) func(context.Context, *reconcile.Plan) (bool, error) {
	return func(_ context.Context, p *reconcile.Plan) (bool, error) {
		if yes {
			return true, nil
		}
		fmt.Fprintf(env.Stdout, "\nthis plan REMOVES DATA:\n")
		for _, s := range p.Describe() {
			if s.Change == reconcile.Delete {
				fmt.Fprintf(env.Stdout, "  %s\n", s.String())
			}
		}
		return TypedConfirm{Subject: name}.Prompt(env.Stdin, env.Stdout)
	}
}

// TypedConfirm is rebuild.sh's prompt, generalised.
//
// The contract is that the operator TYPES THE SUBJECT -- the instance name --
// and that the check happens where the action does, not in the interface that
// asked. A y/n prompt is one keystroke from a mistake and, on the web side,
// one clickjacked frame from somebody else's keystroke; a name that has to be
// typed cannot be supplied by an attacker who does not know which server they
// are looking at, and cannot be answered by muscle memory.
//
// It is exported and holds nothing but data on purpose: phase 15's API edge
// must apply the SAME check to the same field, and a second implementation of
// "did they confirm" is how the browser path and the CLI path end up with two
// different answers.
type TypedConfirm struct {
	// Subject is what must be typed back, verbatim. For every destructive
	// route in v1 that is the instance name.
	Subject string
	// ArchiveAck is set when the caller must also acknowledge that an archive
	// is about to be written and relied on. §16.4 carries it as a separate
	// field from the typed name because they answer different questions:
	// "which server" and "yes, I understand the world is being replaced".
	ArchiveAck bool
}

// ErrNotConfirmed is a destructive action the operator did not confirm.
var ErrNotConfirmed = errors.New("gvm: not confirmed")

// Check is the server-side half, and the only one that decides anything.
//
// typed is what arrived -- from a terminal, from a form field, from a JSON
// body -- and it is compared verbatim. No trimming of case, no "y" shortcut,
// no accepting a prefix: the whole value of the contract is that the string is
// hard to produce by accident.
func (c TypedConfirm) Check(typed string, ack bool) error {
	if c.Subject == "" {
		return fmt.Errorf("%w: nothing to confirm against", ErrNotConfirmed)
	}
	if typed != c.Subject {
		return fmt.Errorf("%w: type %q exactly to confirm", ErrNotConfirmed, c.Subject)
	}
	if c.ArchiveAck && !ack {
		return fmt.Errorf("%w: the archive acknowledgement was not given", ErrNotConfirmed)
	}
	return nil
}

// Prompt asks a terminal and hands the answer to Check.
//
// It reads ONE line and does not loop. A prompt that re-asks is a prompt an
// operator answers by holding a key down, and this one exists to be read.
func (c TypedConfirm) Prompt(in io.Reader, out io.Writer) (bool, error) {
	if in == nil {
		return false, fmt.Errorf("%w: nothing to read a confirmation from", ErrNotConfirmed)
	}
	fmt.Fprintf(out, "\n  type %q to confirm (anything else aborts): ", c.Subject)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false, fmt.Errorf("%w: %v", ErrNotConfirmed, err)
	}
	typed := strings.TrimRight(line, "\r\n")
	if err := c.Check(typed, true); err != nil {
		fmt.Fprintln(out)
		return false, err
	}
	fmt.Fprintln(out)
	return true, nil
}
