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
	sts := lifecycle.Statuses(rep)
	if opt.Problems {
		sts = onlyProblems(sts)
	}
	if opt.JSON {
		if err := writeJSON(env.Stdout, sts); err != nil {
			return err
		}
	} else {
		printList(env.Stdout, sts, rep)
	}
	// `list --problems` that found any exits 4 (checked and found something), so
	// a monitoring script can act on "something is unhealthy" by exit code (U8).
	// A plain `list` always exits 0.
	if opt.Problems && len(sts) > 0 {
		return checked(fmt.Errorf("%d instance(s) need attention", len(sts)))
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
