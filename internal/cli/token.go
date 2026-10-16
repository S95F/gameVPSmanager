package cli

import (
	"context"
	"flag"
	"fmt"
)

func tokenGroup() subGroup {
	return subGroup{Verb: "token", Subs: []subcmd{
		{Name: "create", Summary: "mint an API token", Run: runTokenCreate,
			Examples: []string{"gvm token create --name ci-bot --role operator"}},
		{Name: "list", Summary: "list API tokens", Run: runTokenList},
		{Name: "revoke", Summary: "revoke an API token", Run: runTokenRevoke},
	}}
}

func runToken(ctx context.Context, env *Env, args []string) error {
	return tokenGroup().dispatch(ctx, env, args)
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
		return usageErr(&userError{Msg: "create needs --name", Hint: "a name for the token"})
	}
	_ = role
	return errAdminUnbuilt("token create")
}

func runTokenList(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return errAdminUnbuilt("token list")
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
	return errAdminUnbuilt("token revoke")
}
