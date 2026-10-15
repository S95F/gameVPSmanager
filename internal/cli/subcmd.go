package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/S95F/gameVPSmanager/internal/avail"
)

// subcmd is one entry in a group's subcommand table.
//
// The groups under a verb — `gvm user`, `gvm db`, `gvm adopt` and the rest —
// used to each hand-roll the same `switch`: a usage string here, a dispatch
// there, a default case that printed a bare "unknown subcommand". A table is
// one declaration of the same thing, so the help, the did-you-mean and the
// availability note all read from the one list and cannot drift from the
// dispatch they describe.
type subcmd struct {
	Name    string
	Summary string
	// Avail is this subcommand's availability, so the group's help can show a
	// subcommand that is not in this build rather than letting an operator
	// discover it one refusal at a time. The zero value is Works.
	Avail avail.Availability
	// Examples are whole command lines an operator can paste, shown under the
	// subcommand's own `--help` via the group printer. Optional.
	Examples []string
	Run      func(ctx context.Context, env *Env, args []string) error
}

// subGroup is the set of subcommands under one verb.
type subGroup struct {
	// Verb is the parent command, e.g. "user". Every message reads "gvm <verb>:
	// …", so the sentence an operator sees names the thing they typed.
	Verb string
	Subs []subcmd
}

func (g subGroup) names() []string {
	out := make([]string, len(g.Subs))
	for i, c := range g.Subs {
		out[i] = c.Name
	}
	return out
}

func (g subGroup) lookup(name string) (subcmd, bool) {
	for _, c := range g.Subs {
		if c.Name == name {
			return c, true
		}
	}
	return subcmd{}, false
}

// dispatch routes one `gvm <verb> <sub> …` invocation, with the behaviour the
// six groups used to repeat: no subcommand is a usage error (exit 2) with the
// table on stderr; `-h`/`--help`/`help` prints the table to stdout and exits 0
// (U7); an unknown subcommand is a usage error that suggests the nearest real
// one rather than dumping full usage; anything else runs.
func (g subGroup) dispatch(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		g.printTable(env.Stderr)
		return usageErr(fmt.Errorf("%s: subcommand required", g.Verb))
	}
	sub := args[0]
	if isHelpFlag(sub) {
		g.printTable(env.Stdout)
		return errHelpShown
	}
	c, ok := g.lookup(sub)
	if !ok {
		if s := didYouMean(sub, g.names()); s != "" {
			return usageErr(fmt.Errorf("%s: unknown subcommand %q — did you mean %q?", g.Verb, sub, s))
		}
		return usageErr(fmt.Errorf("%s: unknown subcommand %q", g.Verb, sub))
	}
	return c.Run(ctx, env, args[1:])
}

// printTable prints the subcommand listing, each line carrying the entry's
// availability so the one listing an operator reads first already says which
// subcommands are not in this build.
func (g subGroup) printTable(w io.Writer) {
	fmt.Fprintf(w, "Usage: gvm %s <subcommand>\n\n", g.Verb)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range g.Subs {
		fmt.Fprintf(tw, "  %s\t%s\n", c.Name, subSummary(c))
	}
	_ = tw.Flush()
}

// subSummary appends an availability note to a subcommand's one-liner.
func subSummary(c subcmd) string {
	switch c.Avail.State {
	case avail.NotInBuild:
		if c.Avail.Why != "" {
			return c.Summary + " (" + c.Avail.Why + ")"
		}
		return c.Summary + " (not in this build)"
	case avail.Partial:
		if c.Avail.Why != "" {
			return c.Summary + " (" + c.Avail.Why + ")"
		}
	}
	return c.Summary
}
