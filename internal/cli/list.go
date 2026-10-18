// `gvm list` — every instance this host defines, from /etc and only from /etc.
//
// The enumeration is lifecycle.List over /etc/gvm/instances/*.yaml, never a
// database query. Section 1's promise is that the control plane being dead
// changes nothing about the servers, and a list that needed Postgres would
// break it in the one place an operator most needs it to hold: the moment they
// are trying to find out what is on the box.
//
// A sweep that could not ENUMERATE is an error. A sweep that enumerated and hit
// a problem on one instance is a report with a problem in it, because an empty
// list and a list that could not be built read identically otherwise, and the
// second must never be mistaken for a clean bill of health.

package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

type listOptions struct {
	JSON     bool
	Problems bool
	Remote   string
}

func parseListArgs(env *Env, args []string) (listOptions, error) {
	var opt listOptions
	fs := flag.NewFlagSet("gvm list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm list\n\n")
		fs.PrintDefaults()
	}
	fs.BoolVar(&opt.JSON, "json", false, "print the statuses as JSON")
	fs.BoolVar(&opt.Problems, "problems", false, "list only instances that are not healthy")
	fs.StringVar(&opt.Remote, "remote", "", "dashboard URL to run this against instead of the local host")

	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return listOptions{}, flagErr(err)
	}
	if len(pos) > 0 {
		fs.Usage()
		return listOptions{}, fmt.Errorf("gvm list takes no arguments; did you mean `gvm status %s`?", pos[0])
	}
	return opt, nil
}

func runList(ctx context.Context, env *Env, args []string) error {
	opt, err := parseListArgs(env, args)
	if err != nil {
		return err
	}
	t, err := Resolve(env, "list", opt.Remote)
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

	// The Reconciler, deliberately: it observes every instance and cannot
	// actuate anything (it holds a UnitReader, which has no Start on it). A
	// listing that could restart something is a listing nobody can run twice.
	rc := &lifecycle.Reconciler{Obs: h.observer()}
	rep, err := rc.Sweep(ctx)
	if err != nil {
		return err
	}
	all := lifecycle.Statuses(rep)
	var probs []lifecycle.Status
	if opt.Problems {
		probs = onlyProblems(all)
	}
	switch {
	case opt.JSON && opt.Problems:
		if err := writeJSON(env.Stdout, probs); err != nil {
			return err
		}
	case opt.JSON:
		if err := writeJSON(env.Stdout, all); err != nil {
			return err
		}
	case opt.Problems:
		printProblems(env.Stdout, all, probs, rep)
	default:
		printList(env.Stdout, all, rep)
	}
	// `list --problems` that found any exits 4 (checked and found something), so
	// a monitoring script can act on "something is unhealthy" by exit code (U8).
	// A plain `list` always exits 0.
	if opt.Problems && len(probs) > 0 {
		return checked(fmt.Errorf("%d instance(s) need attention", len(probs)))
	}
	return nil
}

// onlyProblems filters by the phase table's own severity, plus the two
// conditions that are wrong on a host whose phases all look fine: unapplied
// spec drift, and a source the observer could not read.
func onlyProblems(sts []lifecycle.Status) []lifecycle.Status {
	var out []lifecycle.Status
	for _, s := range sts {
		if s.Phase.Severity() == lifecycle.SevProblem || s.Drift || len(s.Problems) > 0 {
			out = append(out, s)
		}
	}
	return out
}

// printProblems is `gvm list --problems`: only the instances that need a
// person, or — when none do — a single line that says so and still accounts for
// the host, so a clean result is a positive statement rather than a blank.
func printProblems(w io.Writer, all, probs []lifecycle.Status, rep lifecycle.SweepReport) {
	if len(probs) == 0 {
		// "no problems" and "no instances" read the same as a blank; this says
		// which, and how many servers were actually checked (day2-ops-14).
		sum := lifecycle.Summarise(all)
		fmt.Fprintf(w, "\nnothing needs attention (%s, %d running)\n",
			plural(len(all), "instance"), sum.Running)
	} else {
		fmt.Fprintln(w)
		for _, s := range probs {
			fmt.Fprintf(w, "  %s\n", s.Line())
		}
		// The summary is of the WHOLE host, not just the problem rows: an
		// operator wants "2 of 6 need attention", not a count of a list they can
		// already see.
		fmt.Fprintf(w, "\n  %s\n", lifecycle.Summarise(all))
		fmt.Fprint(w, "  → gvm status <name> for details; gvm doctor for host checks\n")
	}
	for _, p := range rep.Problems {
		fmt.Fprintf(w, "  ! %s: %s\n", p.Source, strings.TrimSpace(p.Detail))
	}
}

func printList(w io.Writer, sts []lifecycle.Status, rep lifecycle.SweepReport) {
	if len(sts) == 0 {
		fmt.Fprintf(w, "\nno instances. `gvm create <name> --game <id>` defines one.\n")
	} else {
		fmt.Fprintln(w)
		for _, s := range sts {
			fmt.Fprintf(w, "  %s\n", s.Line())
		}
		fmt.Fprintf(w, "\n  %s\n", lifecycle.Summarise(sts))
	}
	for _, p := range rep.Problems {
		// A sweep problem is about the DIRECTORY, not about an instance: a
		// file that is not a legal instance name, a mirror write that failed.
		// It is printed apart from the rows so it cannot be read as one
		// server's trouble.
		fmt.Fprintf(w, "  ! %s: %s\n", p.Source, strings.TrimSpace(p.Detail))
	}
}
