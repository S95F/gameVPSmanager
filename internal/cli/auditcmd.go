package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
)

// runAudit dispatches gvm audit subcommands.
//
// `gvm audit tail` reads the on-disk spool files and replays them in
// chronological order. Postgres connectivity is NOT required -- the spool is
// the durable offline copy. `gvm audit search` is a filtered tail.
func runAudit(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "usage: gvm audit <subcommand>")
		fmt.Fprintln(env.Stderr, "subcommands: tail, search")
		return errors.New("audit: subcommand required")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "-h", "--help", "help":
		fmt.Fprintln(env.Stdout, "usage: gvm audit <subcommand>")
		fmt.Fprintln(env.Stdout, "subcommands: tail, search")
		return errHelpShown
	}
	switch sub {
	case "tail":
		return runAuditTail(ctx, env, rest)
	case "search":
		return runAuditSearch(ctx, env, rest)
	default:
		return fmt.Errorf("audit: unknown subcommand %q", sub)
	}
}

func runAuditTail(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("audit tail", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	n := fs.Int("n", 50, "number of most-recent records to print")
	spoolDir := fs.String("spool-dir", "/var/lib/gvm/spool", "spool directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = n
	_ = spoolDir
	// In the production path this reads the spool files and replays them.
	// Without a running gvmd (or when using the spool directly), this works
	// offline. The full implementation needs audit.ListSpools + audit.Replay.
	return errAdminUnbuilt("audit tail")
}

func runAuditSearch(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("audit search", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	action := fs.String("action", "", "filter by action, e.g. instance.stop")
	instance := fs.String("instance", "", "filter by instance name")
	actor := fs.String("actor", "", "filter by actor email or token prefix")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = action
	_ = instance
	_ = actor
	return errAdminUnbuilt("audit search")
}
