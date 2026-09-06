// `gvm status` — one instance, as the phase table sees it.
//
// Everything printed here is a projection of one Observation and the Verdict
// derived from it. Nothing is computed in this file, and that is the reason
// `gvm status`, `gvm list` and (later) `gvm doctor` cannot tell an operator
// three different stories about one server: there is one phase table, and
// these are three views of its answer.
//
// The --json shape is lifecycle.Status, whose field names are the columns of
// section 14.4's instance_status. A dashboard reading this file's output and a
// dashboard reading the mirror therefore see the same keys, which is what stops
// the API growing a second vocabulary for the same facts.

package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

type statusOptions struct {
	Name   string
	JSON   bool
	Remote string
}

func parseStatusArgs(env *Env, args []string) (statusOptions, error) {
	var opt statusOptions
	fs := flag.NewFlagSet("gvm status", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm status <instance>\n\n")
		fs.PrintDefaults()
	}
	fs.BoolVar(&opt.JSON, "json", false, "print lifecycle.Status as JSON")
	fs.StringVar(&opt.Remote, "remote", "", "dashboard URL to run this against instead of the local host")

	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return statusOptions{}, flagErr(err)
	}
	name, err := oneInstance(pos)
	if err != nil {
		fs.Usage()
		return statusOptions{}, err
	}
	opt.Name = name
	return opt, nil
}

func runStatus(ctx context.Context, env *Env, args []string) error {
	opt, err := parseStatusArgs(env, args)
	if err != nil {
		return err
	}
	t, err := Resolve(env, "status", opt.Remote)
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

	obs, err := h.observer().Observe(ctx, opt.Name)
	if err != nil {
		return err
	}
	st := lifecycle.NewStatus(obs, lifecycle.Derive(obs))
	if opt.JSON {
		return writeJSON(env.Stdout, st)
	}
	fmt.Fprintln(env.Stdout)
	fmt.Fprint(env.Stdout, st.Report())
	printObservationGaps(env, obs)
	return nil
}

// printObservationGaps says what could NOT be established.
//
// An Observation collects its failures rather than returning them, because a
// status.json that will not parse should still let systemd's view be reported.
// The corollary is that somebody has to print them: a missing source that
// silently becomes an absent fact is how "not checked" gets read as "fine".
func printObservationGaps(env *Env, obs lifecycle.Observation) {
	for _, p := range obs.Problems {
		if p.Path != "" {
			fmt.Fprintf(env.Stdout, "  ?       %s: %s (%s)\n", p.Source, p.Detail, p.Path)
			continue
		}
		fmt.Fprintf(env.Stdout, "  ?       %s: %s\n", p.Source, p.Detail)
	}
}
