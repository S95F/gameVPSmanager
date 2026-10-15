package cli

import (
	"context"
	"flag"
)

// runAudit dispatches gvm audit subcommands.
//
// `gvm audit tail` reads the on-disk spool files and replays them in
// chronological order. Postgres connectivity is NOT required -- the spool is
// the durable offline copy. `gvm audit search` is a filtered tail.
func runAudit(ctx context.Context, env *Env, args []string) error {
	return subGroup{Verb: "audit", Subs: []subcmd{
		{Name: "tail", Summary: "replay the audit spool in order", Run: runAuditTail},
		{Name: "search", Summary: "filter the audit spool", Run: runAuditSearch},
	}}.dispatch(ctx, env, args)
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
