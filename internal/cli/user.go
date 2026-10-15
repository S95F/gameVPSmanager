package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/avail"
)

// runUser dispatches gvm user subcommands.
// All mutating operations require gvmd (privileged verb gateway).
func runUser(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "usage: gvm user <subcommand>")
		fmt.Fprintln(env.Stderr, "subcommands: list, create, show, set-role, set-password, disable, enable, reset-totp")
		return usageErr(errors.New("user: subcommand required"))
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "-h", "--help", "help":
		fmt.Fprintln(env.Stdout, "usage: gvm user <subcommand>")
		fmt.Fprintln(env.Stdout, "subcommands: list, create, show, set-role, set-password, disable, enable, reset-totp")
		return errHelpShown
	}
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
		return usageErr(fmt.Errorf("user: unknown subcommand %q", sub))
	}
}

func runUserList(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return errAdminUnbuilt("user list")
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
	return errAdminUnbuilt("user create")
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
	return errAdminUnbuilt("user show")
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
	return errAdminUnbuilt("user set-role")
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
	return errAdminUnbuilt("user set-password")
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
	return errAdminUnbuilt("user disable")
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
	return errAdminUnbuilt("user enable")
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
	return errAdminUnbuilt("user reset-totp")
}

// errAdminUnbuilt is the refusal the admin groups share.
//
// The command is routed, so `--help` and argument validation already ran; what
// is missing is the engine behind it — dashboard users, API tokens, the secret
// store or the audit reader. It returns an avail error so Main prints the one
// standard line and exits 3 (U8), and it NEVER names gvmd: an operator who does
// not have the daemon must not be sent to debug a unit that was never shipped
// (U10). The op prefix only chooses which sentence, so the reason is specific
// without the caller repeating itself.
func errAdminUnbuilt(op string) error {
	switch {
	case strings.HasPrefix(op, "user"):
		return avail.New("dashboard users, roles and passwords arrive with the dashboard")
	case strings.HasPrefix(op, "token"):
		return avail.New("API tokens arrive with the dashboard")
	case strings.HasPrefix(op, "secret"):
		return avail.New("storing and reading instance secrets arrives in a later release")
	case strings.HasPrefix(op, "audit"):
		return avail.New("reading the audit log arrives with the audit spool reader")
	}
	return avail.New("that part of gvm is not in this build")
}
