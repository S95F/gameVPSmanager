// `gvm update` — decide before you stop (D-43), from the terminal.
//
// The command is `update.sh` with its one structural flaw removed. That script
// stops the server, runs `app_update … validate`, starts it again and sleeps
// 180 seconds, every night, whether or not anything changed. Here phases 1 and
// 2 are network READS performed with players online, the decision between them
// and the stop is a pure function (jobs.Decide), and on a night when nothing
// moved the run records `skipped` having issued zero stop calls.
//
// `--diff` is that decision, printed and nothing else. It is the phase list
// this instance would execute and which of those phases may take the server
// down -- read off the step table itself through jobs.StepNames, so the list an
// operator is shown is the list the job will run rather than a paragraph that
// has to be kept in step with it.
//
// `--repair` is the only path in gvm that emits `validate` (D-32), and it is a
// flag and never an escalation: a failed post-update verification sets
// NEEDS_REPAIR, starts the server anyway and tells the operator to run this.
// `--repair --dry-run` prints the files `validate` may overwrite without
// overwriting any of them, which is the half an operator with customised
// configs actually needs.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/jobs"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/update"
)

func runUpdate(ctx context.Context, env *Env, args []string) error {
	var opt jobOptions
	var diff, repair, dry bool
	fs := flag.NewFlagSet("gvm update", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm update <instance> [--diff] [--repair [--dry-run]]\n\n"+
			"Checks for a Steam build change and a content change with the server UP,\n"+
			"and stops it only if one of them needs it. On a quiet night nothing is\n"+
			"touched and the run records `skipped`, which is a success (D-43).\n\n")
		fs.PrintDefaults()
	}
	opt.bind(fs, jobs.TriggerCLI)
	fs.BoolVar(&diff, "diff", false,
		"print the phases this update would run, and which of them may stop the server; "+
			"touches nothing")
	fs.BoolVar(&repair, "repair", false,
		"emit `validate`: re-download every file that differs from what the provider wrote. "+
			"It OVERWRITES customised files and takes a preserve archive first (D-32)")
	fs.BoolVar(&dry, "dry-run", false,
		"with --repair, print the at-risk files and stop")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	name, err := oneInstance(pos)
	if err != nil {
		fs.Usage()
		return err
	}
	switch {
	case diff && repair:
		return errors.New("--diff and --repair are different operations: one reads, the other " +
			"rewrites the install tree")
	case dry && !repair:
		return errors.New("--dry-run belongs to --repair; `gvm update <n> --diff` is the " +
			"read-only view of an ordinary update")
	case repair:
		return runUpdateRepair(ctx, env, name, opt, dry)
	case diff:
		return runUpdateDiff(ctx, env, name, opt)
	}
	return runKind(ctx, env, "update", lifecycle.JobUpdate, name, opt)
}

// runUpdateDiff prints the step table without running it.
//
// From jobs.StepNames and jobs.StoppingSteps rather than from a list in this
// file: a phase added to the runner's table next year appears here the day it
// is added, and a phase that may stop the server appears in the second list
// because the table says `Stops: true` -- which is the same declaration the
// engine's own assertion reads.
func runUpdateDiff(ctx context.Context, env *Env, name string, opt jobOptions) error {
	t, err := Resolve(env, "update", opt.Remote)
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
	jenv, err := buildJobEnv(ctx, env, h, jobs.Resolved{
		Slug:   jobs.Slug{Instance: name, Job: "update"},
		Kind:   lifecycle.JobUpdate,
		Action: "update",
		Via:    jobs.ViaJobName,
	}, opt)
	if err != nil {
		return err
	}
	names, err := jobs.StepNames(jenv)
	if err != nil {
		return err
	}
	stops, err := jobs.StoppingSteps(jenv)
	if err != nil {
		return err
	}
	if opt.JSON {
		return writeJSON(env.Stdout, map[string]any{
			"instance": name, "managed_install": jenv.ManagedInstall,
			"phases": names, "may_stop": stops,
		})
	}
	printUpdateDiff(env.Stdout, name, jenv.ManagedInstall, names, stops)
	return nil
}

func printUpdateDiff(w io.Writer, name string, managed bool, names, stops []string) {
	fmt.Fprintf(w, "\nupdate %s — the phases, and nothing run\n", name)
	stopping := map[string]bool{}
	for _, s := range stops {
		stopping[s] = true
	}
	for i, n := range names {
		mark := "   "
		if stopping[n] {
			mark = "!! "
		}
		fmt.Fprintf(w, "  %s%2d %s\n", mark, i+1, n)
	}
	fmt.Fprintf(w, "\n  !! may stop the server, and only if the decision says so\n")
	if !managed {
		// The adopted case, said before it happens rather than discovered in
		// a summary field: gvm may write nothing into that tree, so a newer
		// build is REPORTED and never installed.
		fmt.Fprintf(w, "  %s is adopted (managed_install: false): a newer Steam build is "+
			"reported as update_available and never installed. Taking over binary updates for "+
			"an adopted instance is not available in this build\n", name)
	}
}

// runUpdateRepair is D-32's response to a failed verification, and the only
// path that emits `validate`.
//
// Confirmed comes from the flag and from nowhere else. internal/update refuses
// the request without it, prints the at-risk list from the provider's own
// record before anything runs, and takes a verified `preserve` archive first --
// because `validate` overwrites files an operator customised, and Valve says
// so explicitly.
func runUpdateRepair(ctx context.Context, env *Env, name string, opt jobOptions, dry bool) error {
	t, err := Resolve(env, "update", opt.Remote)
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
	pipe, err := h.repairPipeline(ctx, name, opt)
	if err != nil {
		return err
	}
	reason := strings.TrimSpace(opt.Reason)
	if reason == "" {
		reason = "operator ran gvm update --repair"
	}
	rep, rerr := pipe.Repair(ctx, update.RepairRequest{
		Confirmed: true, Reason: reason, DryRun: dry,
	})
	if rep != nil {
		printRepair(env.Stdout, name, rep)
	}
	return rerr
}

func printRepair(w io.Writer, name string, rep *update.RepairReport) {
	what := "repair"
	if rep.DryRun {
		what = "repair --dry-run"
	}
	fmt.Fprintf(w, "\n%s %s\n", what, name)
	switch {
	case !rep.AtRiskKnown:
		// "Nothing differs" and "nobody looked" are different answers, and on
		// this path the difference decides whether to proceed.
		fmt.Fprint(w, "  at risk   UNKNOWN: this provider keeps no record of what it wrote, "+
			"so gvm cannot say which files `validate` would overwrite\n")
	case len(rep.AtRisk) == 0:
		fmt.Fprint(w, "  at risk   nothing: every file in the install tree matches what the "+
			"provider wrote\n")
	default:
		fmt.Fprintf(w, "  at risk   %d file(s) `validate` may overwrite:\n", len(rep.AtRisk))
		for _, f := range rep.AtRisk {
			fmt.Fprintf(w, "              %s\n", f)
		}
	}
	if rep.Archive != nil {
		fmt.Fprintf(w, "  archive   %s (preserve, verified)\n", rep.Archive.Path)
	}
	for _, s := range rep.Steps {
		fmt.Fprintf(w, "  step      %s\n", s)
	}
	if rep.DryRun {
		fmt.Fprintf(w, "\n  Nothing was written. `gvm update %s --repair` performs it.\n", name)
	}
}
