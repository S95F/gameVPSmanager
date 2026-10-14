package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
)

// runUser dispatches gvm user subcommands.
// All mutating operations require gvmd (privileged verb gateway).
func runUser(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "usage: gvm user <subcommand>")
		fmt.Fprintln(env.Stderr, "subcommands: list, create, show, set-role, set-password, disable, enable, reset-totp")
		return errors.New("user: subcommand required")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "list":
		return runUserList(ctx, env, rest)
	case "create":
		return runUserCreate(ctx, env, rest)
	case "show":
		return runUserShow(ctx, env, rest)
	case "set-role":
		return runUserSetRole(ctx, env, rest)
	case "set-password":
		return runUserSetPassword(ctx, env, rest)
	case "disable":
		return runUserDisable(ctx, env, rest)
	case "enable":
		return runUserEnable(ctx, env, rest)
	case "reset-totp":
		return runUserResetTOTP(ctx, env, rest)
	default:
		return fmt.Errorf("user: unknown subcommand %q", sub)
	}
}

func runUserList(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return errRequiresDaemon("user list")
}

func runUserCreate(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	email := fs.String("email", "", "user email address (required)")
	role := fs.String("role", "viewer", "global role: viewer|operator|admin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return fmt.Errorf("user create: --email is required")
	}
	_ = role
	return errRequiresDaemon("user create")
}

func runUserShow(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user show", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("user show: <email> required")
	}
	return errRequiresDaemon("user show")
}

func runUserSetRole(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user set-role", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("user set-role: <email> <role> required")
	}
	return errRequiresDaemon("user set-role")
}

func runUserSetPassword(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user set-password", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("user set-password: <email> required")
	}
	return errRequiresDaemon("user set-password")
}

func runUserDisable(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user disable", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("user disable: <email> required")
	}
	return errRequiresDaemon("user disable")
}

func runUserEnable(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user enable", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("user enable: <email> required")
	}
	return errRequiresDaemon("user enable")
}

func runUserResetTOTP(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user reset-totp", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("user reset-totp: <email> required")
	}
	return errRequiresDaemon("user reset-totp")
}

// errRequiresDaemon returns a clear error explaining that the operation needs
// the gvmd privileged verb gateway. It is a distinct error so callers can check
// for it in tests without matching a string.
func errRequiresDaemon(op string) error {
	return fmt.Errorf("%s: requires gvmd — ensure the daemon is running (`systemctl status gvmd`)", op)
}
