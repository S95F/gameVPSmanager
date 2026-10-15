package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/avail"
)

// runUser dispatches gvm user subcommands.
// All mutating operations require gvmd (privileged verb gateway).
func runUser(ctx context.Context, env *Env, args []string) error {
	return subGroup{Verb: "user", Subs: []subcmd{
		{Name: "list", Summary: "list dashboard users", Run: runUserList},
		{Name: "create", Summary: "create a dashboard user", Run: runUserCreate},
		{Name: "show", Summary: "show one user", Run: runUserShow},
		{Name: "set-role", Summary: "set a user's global role", Run: runUserSetRole},
		{Name: "set-password", Summary: "set a user's password", Run: runUserSetPassword},
		{Name: "disable", Summary: "disable a user", Run: runUserDisable},
		{Name: "enable", Summary: "enable a user", Run: runUserEnable},
		{Name: "reset-totp", Summary: "clear a user's TOTP enrolment", Run: runUserResetTOTP},
	}}.dispatch(ctx, env, args)
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
