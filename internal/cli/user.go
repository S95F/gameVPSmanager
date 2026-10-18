package cli

import (
	"context"
	"flag"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/avail"
)

// userGroup is the `gvm user` subcommand table. All mutating operations
// require gvmd (the privileged verb gateway).
func userGroup() subGroup {
	return subGroup{Verb: "user", Subs: []subcmd{
		{Name: "list", Summary: "list dashboard users", Run: runUserList},
		{Name: "create", Summary: "create a dashboard user", Run: runUserCreate,
			Examples: []string{"gvm user create --email ops@example.com --role operator"}},
		{Name: "show", Summary: "show one user", Run: runUserShow},
		{Name: "set-role", Summary: "set a user's global role", Run: runUserSetRole},
		{Name: "set-password", Summary: "set a user's password", Run: runUserSetPassword},
		{Name: "disable", Summary: "disable a user", Run: runUserDisable},
		{Name: "enable", Summary: "enable a user", Run: runUserEnable},
		{Name: "reset-totp", Summary: "clear a user's TOTP enrolment", Run: runUserResetTOTP},
	}}
}

func runUser(ctx context.Context, env *Env, args []string) error {
	return userGroup().dispatch(ctx, env, args)
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
		return usageErr(&userError{Msg: "create needs --email", Hint: "the new user's email address"})
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
		return usageErr(&userError{Msg: "show needs an email", Hint: "the user to show"})
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
		return usageErr(&userError{Msg: "set-role needs <email> <role>", Hint: "the user, then viewer|operator|admin"})
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
		return usageErr(&userError{Msg: "set-password needs an email", Hint: "the user whose password to set"})
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
		return usageErr(&userError{Msg: "disable needs an email", Hint: "the user to disable"})
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
		return usageErr(&userError{Msg: "enable needs an email", Hint: "the user to enable"})
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
		return usageErr(&userError{Msg: "reset-totp needs an email", Hint: "the user whose TOTP enrolment to clear"})
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
