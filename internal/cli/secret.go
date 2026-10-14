package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
)

func runSecret(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "usage: gvm secret <subcommand>")
		fmt.Fprintln(env.Stderr, "subcommands: list, set, rotate, show-path")
		return errors.New("secret: subcommand required")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "list":
		return runSecretList(ctx, env, rest)
	case "set":
		return runSecretSet(ctx, env, rest)
	case "rotate":
		return runSecretRotate(ctx, env, rest)
	case "show-path":
		return runSecretShowPath(ctx, env, rest)
	default:
		return fmt.Errorf("secret: unknown subcommand %q", sub)
	}
}

func runSecretList(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secret list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	instance := fs.String("instance", "", "filter by instance name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = instance
	return errRequiresDaemon("secret list")
}

func runSecretSet(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secret set", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	instance := fs.String("instance", "", "instance name (or omit for host scope)")
	name := fs.String("name", "", "secret name, e.g. rcon (required)")
	generate := fs.Bool("generate", false, "generate a new random value")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("secret set: --name is required")
	}
	_ = instance
	_ = generate
	return errRequiresDaemon("secret set")
}

func runSecretRotate(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secret rotate", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	instance := fs.String("instance", "", "instance name")
	name := fs.String("name", "", "secret name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("secret rotate: --name is required")
	}
	_ = instance
	return errRequiresDaemon("secret rotate")
}

func runSecretShowPath(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secret show-path", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	instance := fs.String("instance", "", "instance name")
	name := fs.String("name", "", "secret name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("secret show-path: --name is required")
	}
	_ = instance
	return errRequiresDaemon("secret show-path")
}
