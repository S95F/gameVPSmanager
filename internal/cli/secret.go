package cli

import (
	"context"
	"flag"
)

func secretGroup() subGroup {
	return subGroup{Verb: "secret", Subs: []subcmd{
		{Name: "list", Summary: "list instance secret names", Run: runSecretList},
		{Name: "set", Summary: "set an instance secret", Run: runSecretSet},
		{Name: "rotate", Summary: "rotate an instance secret", Run: runSecretRotate},
		{Name: "show-path", Summary: "print where a secret is stored", Run: runSecretShowPath},
	}}
}

func runSecret(ctx context.Context, env *Env, args []string) error {
	return secretGroup().dispatch(ctx, env, args)
}

// The secret store is not in this build (it lands in a later release), so every
// subcommand here refuses the same way: parse flags, so `--help` exits 0 and a
// typed flag is not "undefined", then return the shared refusal and change
// nothing. The refusal comes BEFORE any required-argument check, so an operator
// learns the engine is missing rather than being nagged about a flag for a
// command that could not act either way — and so the exit code is 3 (not
// available), never 2 (usage). No value is ever read from argv or printed (U9).

func runSecretList(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secret list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.String("instance", "", "filter by instance name")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	return errAdminUnbuilt("secret list")
}

func runSecretSet(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secret set", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.String("instance", "", "instance name (or omit for host scope)")
	fs.String("name", "", "secret name, e.g. rcon")
	fs.Bool("generate", false, "generate a new random value")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	return errAdminUnbuilt("secret set")
}

func runSecretRotate(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secret rotate", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.String("instance", "", "instance name")
	fs.String("name", "", "secret name")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	return errAdminUnbuilt("secret rotate")
}

func runSecretShowPath(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secret show-path", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.String("instance", "", "instance name")
	fs.String("name", "", "secret name")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	return errAdminUnbuilt("secret show-path")
}
