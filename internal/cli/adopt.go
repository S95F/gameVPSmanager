package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/adopt"
)

func runAdopt(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "usage: gvm adopt <subcommand>")
		fmt.Fprintln(env.Stderr, "subcommands: discover, cutover, rollback, finalize")
		return fmt.Errorf("adopt: subcommand required")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "discover":
		return runAdoptDiscover(ctx, env, rest)
	case "cutover":
		fmt.Fprintln(env.Stderr, "adopt cutover: not yet implemented in this phase (requires Linux)")
		return fmt.Errorf("adopt cutover: not yet implemented")
	case "rollback":
		fmt.Fprintln(env.Stderr, "adopt rollback: run the rollback script printed by cutover")
		return fmt.Errorf("adopt rollback: not yet implemented")
	case "finalize":
		return runAdoptFinalize(ctx, env, rest)
	default:
		return fmt.Errorf("adopt: unknown subcommand %q", sub)
	}
}

func runAdoptDiscover(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("adopt discover", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	unitName := fs.String("unit", "zomboid", "bare systemd unit name (without .service)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	discEnv := adopt.OSDiscoverEnv("")
	r, err := adopt.DiscoverPZ(discEnv, *unitName)
	if err != nil {
		return fmt.Errorf("adopt discover: %w", err)
	}

	fmt.Fprint(env.Stdout, adopt.FormatDiscoverResult(r))

	// Exit non-zero if there are any errors in the findings.
	for _, f := range r.Findings {
		if f.Severity == "error" {
			return fmt.Errorf("adopt discover: %d finding(s) require attention before cutover",
				countErrors(r.Findings))
		}
	}
	return nil
}

func runAdoptFinalize(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("adopt finalize", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	force := fs.Bool("force", false, "bypass the 7-day review window")
	serverName := fs.String("name", "", "server name (required)")
	unitPath := fs.String("unit-path", "", "path to legacy unit file (required)")
	adoptedFrom := fs.String("adopted-from", "", "adopted_from metadata value from instance YAML")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *serverName == "" || *unitPath == "" {
		return fmt.Errorf("adopt finalize: --name and --unit-path are required")
	}

	err := adopt.FinalizePZ(*serverName, *unitPath, *adoptedFrom,
		adopt.FinalizeOptions{Force: *force})
	if err != nil {
		if errors.Is(err, adopt.ErrFinalizeNotReady) {
			fmt.Fprintln(env.Stderr, err)
			return err
		}
		return fmt.Errorf("adopt finalize: %w", err)
	}
	fmt.Fprintf(env.Stdout, "finalize: %s legacy unit archived and removed\n", *serverName)
	return nil
}

func countErrors(findings []adopt.DiscoverFinding) int {
	n := 0
	for _, f := range findings {
		if f.Severity == "error" {
			n++
		}
	}
	return n
}
