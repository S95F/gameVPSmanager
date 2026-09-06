// `gvm start` — and the shape every lifecycle verb shares.
//
// The three verbs are one function with three arguments because the
// interesting part is not any of them: it is that every one of them observes
// first, checks a precondition table an operator can read, acts through
// hostops, and observes again. lifecycle.Manager owns all four steps, so this
// file is argument parsing and printing, and there is deliberately nothing
// here that could decide whether a start is allowed.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

type lifecycleOptions struct {
	Name   string
	Reason string
	Force  bool
	Budget time.Duration
	JSON   bool
	Remote string
}

func parseLifecycleArgs(env *Env, verb string, args []string) (lifecycleOptions, error) {
	var opt lifecycleOptions
	fs := flag.NewFlagSet("gvm "+verb, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: gvm %s <instance> [--reason \"...\"]\n\n", verb)
		fs.PrintDefaults()
	}
	fs.StringVar(&opt.Reason, "reason", "", "why, recorded in the audit row and (for a stop) in the hold file")
	fs.BoolVar(&opt.Force, "force", false, "override a refusal that says it can be overridden")
	fs.DurationVar(&opt.Budget, "budget", 0, "bound the shutdown ladder (default: the instance's shutdown.budget)")
	fs.BoolVar(&opt.JSON, "json", false, "print the result as JSON")
	fs.StringVar(&opt.Remote, "remote", "", "dashboard URL to run this against instead of the local host")

	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return lifecycleOptions{}, flagErr(err)
	}
	name, err := oneInstance(pos)
	if err != nil {
		fs.Usage()
		return lifecycleOptions{}, err
	}
	opt.Name = name
	return opt, nil
}

func runStart(ctx context.Context, env *Env, args []string) error {
	return runLifecycle(ctx, env, lifecycle.VerbStart, args)
}

func runLifecycle(ctx context.Context, env *Env, verb lifecycle.Verb, args []string) error {
	opt, err := parseLifecycleArgs(env, string(verb), args)
	if err != nil {
		return err
	}
	t, err := Resolve(env, string(verb), opt.Remote)
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

	mgr := &lifecycle.Manager{
		Obs:      h.observer(),
		Host:     h.Ops,
		Shutdown: h.shutdown(),
		Now:      h.Now,
	}
	res, err := mgr.Do(ctx, lifecycle.Request{
		Verb:     verb,
		Instance: opt.Name,
		Actor:    actorFor(t),
		Reason:   opt.Reason,
		Budget:   opt.Budget,
		Force:    opt.Force,
	})
	if err != nil {
		return lifecycleError(verb, opt, err)
	}
	if opt.JSON {
		return writeJSON(env.Stdout, res)
	}
	printLifecycleResult(env.Stdout, res)
	return nil
}

// lifecycleError turns a refusal into the sentence an operator can act on.
//
// A refusal that can be overridden says so, once, and names the flag; a
// refusal that cannot says why not. Printing "--force is available" next to a
// pin mismatch would be actively harmful: the runner exits 64 a second later
// and the operator would have swapped a clear message for a unit stuck in
// "restart prevented".
func lifecycleError(verb lifecycle.Verb, opt lifecycleOptions, err error) error {
	var ref *lifecycle.Refusal
	if errors.As(err, &ref) && ref.Force && !opt.Force {
		return fmt.Errorf("%w\n  this refusal can be overridden: gvm %s %s --force",
			err, verb, opt.Name)
	}
	if errors.Is(err, lifecycle.ErrNoLadder) {
		return fmt.Errorf("%w\n  a stop that cannot report the rung it ended on cannot say "+
			"whether the world was saved, and audit.instance_stop requires it", err)
	}
	return err
}

func printLifecycleResult(w io.Writer, res *lifecycle.Result) {
	fmt.Fprintf(w, "\n%s %s\n", res.Verb, res.Instance)
	fmt.Fprintf(w, "  before  %s\n", res.Before.Phase)
	fmt.Fprintf(w, "  after   %s\n", res.After.Phase)
	if res.After.Reason != "" {
		fmt.Fprintf(w, "          %s\n", res.After.Reason)
	}
	if res.After.Hint != "" {
		fmt.Fprintf(w, "       -> %s\n", res.After.Hint)
	}
	if res.Forced {
		fmt.Fprintf(w, "  forced  a refusal was overridden; `gvm status %s` will say so too\n",
			res.Instance)
	}
	if s := res.Shutdown; s != nil {
		// The rung is the whole product feature of section 11.6: "the server
		// stopped" and "the server SAVED" were byte-identical in the bash
		// suite and must never be again.
		fmt.Fprintf(w, "  ladder  ended on %s after %s\n", s.Rung, s.Took.Round(time.Millisecond))
		if s.Degraded {
			fmt.Fprintf(w, "          DEGRADED: a SIGKILL was needed, so the world may not have saved\n")
		}
	}
	for _, st := range res.Steps {
		mark := "="
		if st.Changed {
			mark = "~"
		}
		obj := st.Unit
		if obj == "" {
			obj = st.Path
		}
		fmt.Fprintf(w, "  %s %s %s\n", mark, st.Verb, obj)
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
