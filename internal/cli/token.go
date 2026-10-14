package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
)

func runToken(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "usage: gvm token <subcommand>")
		fmt.Fprintln(env.Stderr, "subcommands: create, list, revoke")
		return errors.New("token: subcommand required")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "create":
		return runTokenCreate(ctx, env, rest)
	case "list":
		return runTokenList(ctx, env, rest)
	case "revoke":
		return runTokenRevoke(ctx, env, rest)
	default:
		return fmt.Errorf("token: unknown subcommand %q", sub)
	}
}

func runTokenCreate(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	name := fs.String("name", "", "token name (required)")
	role := fs.String("role", "viewer", "role ceiling: viewer|operator|admin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("token create: --name is required")
	}
	_ = role
	return errRequiresDaemon("token create")
}

func runTokenList(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return errRequiresDaemon("token list")
}

func runTokenRevoke(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("token revoke: <token-prefix> required")
	}
	return errRequiresDaemon("token revoke")
}
