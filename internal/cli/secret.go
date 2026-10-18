// `gvm secret` — set, rotate, list and read back the credentials a game or the
// host needs. Root-CLI only and in-process (RouteHostOp): the verbs read and
// write /etc/gvm/secrets directly through secrets.Store and never touch
// admin.sock. A value never comes from argv (U9) and is never printed except by
// `secret show`, which reveals one value to an operator at an interactive
// terminal and audits the read.
//
// Phase 29 item 1. The declarative `secrets:` block, passworded-by-default
// `create` and the doctor credential checks (items 2–7) are later work; this is
// the surface an operator uses to put a value into the store by hand.

package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/secrets"
)

func secretGroup() subGroup {
	return subGroup{Verb: "secret", Subs: []subcmd{
		{Name: "set", Summary: "set an instance or host secret from a prompt, file, pipe or --generate", Run: runSecretSet,
			Examples: []string{
				"printf '%s' hunter2 | sudo gvm secret set pz1 join_password",
				"sudo gvm secret set cs2-1 gslt",
				"sudo gvm secret set pz1 rcon_password --generate",
				"printf '%s' \"$KEY\" | sudo gvm secret set --host curseforge",
			}},
		{Name: "list", Summary: "list an instance's or the host's secrets, with length and fingerprint", Run: runSecretList,
			Examples: []string{"sudo gvm secret list pz1", "sudo gvm secret list --host"}},
		{Name: "show", Summary: "print one secret's value to an interactive terminal", Run: runSecretShow,
			Examples: []string{"sudo gvm secret show pz1 join_password"}},
		{Name: "rotate", Summary: "replace an instance secret with a freshly generated password", Run: runSecretRotate,
			Examples: []string{"sudo gvm secret rotate pz1 rcon_password"}},
		{Name: "show-path", Summary: "print the file a secret is (or would be) stored in", Run: runSecretShowPath,
			Examples: []string{"sudo gvm secret show-path pz1 join_password"}},
	},
		Note: "A secret value never comes from the command line (it would land in ps and the shell\n" +
			"history); `set` reads it from a no-echo prompt, a pipe, --from-file or --generate.\n" +
			"Instance secrets reject characters a game cannot carry in argv or a config file.",
	}
}

func runSecret(ctx context.Context, env *Env, args []string) error {
	return secretGroup().dispatch(ctx, env, args)
}

// secretHost resolves the transport, requires local root and builds the host
// seams every secret verb shares. Each verb calls it after parsing its flags,
// so `--help` still exits 0 and a bad flag is a usage error before the
// transport is touched.
func secretHost(ctx context.Context, env *Env) (*HostEnv, error) {
	t, err := Resolve(env, "secret", "")
	if err != nil {
		return nil, err
	}
	if err := t.RequireLocal(); err != nil {
		return nil, err
	}
	return env.host(ctx)
}

// secretRef turns `--host`/positionals into a validated ref. A host secret is
// named by one positional; an instance secret by two (instance, name). A ref
// that does not validate is a usage error, with the store's own reason.
func secretRef(host bool, pos []string) (secrets.Ref, error) {
	var ref secrets.Ref
	if host {
		switch len(pos) {
		case 1:
			ref = secrets.HostRef(pos[0])
		case 0:
			return secrets.Ref{}, usageErr(&userError{Msg: "name the host secret",
				Hint: "e.g. sudo gvm secret set --host curseforge"})
		default:
			return secrets.Ref{}, usageErr(&userError{Msg: "a host secret takes one name",
				Hint: "the scope is --host; give just the secret's name"})
		}
	} else {
		switch len(pos) {
		case 2:
			ref = secrets.InstanceRef(pos[0], pos[1])
		case 0, 1:
			return secrets.Ref{}, usageErr(&userError{Msg: "name the instance and the secret",
				Hint: "e.g. sudo gvm secret set pz1 join_password (or --host for a box-wide secret)"})
		default:
			return secrets.Ref{}, usageErr(&userError{Msg: "too many arguments",
				Hint: "give one instance and one secret name"})
		}
	}
	if err := ref.Validate(); err != nil {
		return secrets.Ref{}, usageErr(err)
	}
	return ref, nil
}

// effectHint is the line `set` and `rotate` print under the fingerprint: a
// stored value does nothing until the game reads it, and nothing restarts on
// its own (rule 4). Where the game reads each secret (argv, env or a config
// file) is the declarative `secrets:` block of Phase 29 item 2, which is not in
// this build; until it lands, the honest line names the restart and stops
// short of claiming the value has reached the game.
func effectHint(ref secrets.Ref) string {
	if ref.Scope == secrets.ScopeHost {
		return "takes effect the next time a client reads it; nothing to restart."
	}
	return "takes effect at next restart: sudo gvm restart " + ref.Key
}

func runSecretSet(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secret set", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	host := fs.Bool("host", false, "set a box-wide secret (e.g. an API key) rather than one instance's")
	generate := fs.Bool("generate", false, "generate a random base62 password instead of reading one")
	length := fs.Int("length", secrets.DefaultPasswordLen, "length of a --generate password")
	fromFile := fs.String("from-file", "", "read the value from this file instead of prompting")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	if *generate && *fromFile != "" {
		return usageErr(&userError{Msg: "--generate and --from-file both supply a value",
			Hint: "pick one"})
	}

	ref, err := secretRef(*host, pos)
	if err != nil {
		return err
	}

	value, source, err := secretValue(env, ref, *generate, *length, *fromFile)
	if err != nil {
		return err
	}

	// Rule 10: an instance secret reaches a game's argv or config files, so it
	// is refused for a character that would be cut, escaped or re-interpreted.
	// A host secret keeps only the store's file-format rules. A refusal is exit
	// 5 and writes nothing.
	if ref.Scope == secrets.ScopeInstance {
		err = secrets.ValidateInstanceValue(value)
	} else {
		err = secrets.ValidateValue(value)
	}
	if err != nil {
		return refused(fmt.Errorf("%s: %w", ref, err))
	}

	store, err := secretStoreFor(ctx, env)
	if err != nil {
		return err
	}
	return writeSecret(ctx, env, store, ref, value, source, audit.SecretWrite)
}

// secretValue produces the value and its provenance from the chosen source:
// --generate mints one, --from-file reads one (trimming a single trailing EOL
// an editor leaves), otherwise it is read from a no-echo prompt at a terminal
// or one line of a pipe. It never reads from argv.
func secretValue(env *Env, ref secrets.Ref, generate bool, length int, fromFile string) (secrets.Value, secrets.Source, error) {
	switch {
	case generate:
		v, err := secrets.GeneratePassword(length)
		if err != nil {
			return secrets.Value{}, "", usageErr(err)
		}
		return v, secrets.SourceGenerated, nil
	case fromFile != "":
		b, err := os.ReadFile(fromFile)
		if err != nil {
			return secrets.Value{}, "", fmt.Errorf("secret set: %w", err)
		}
		return secrets.New(trimOneTrailingEOL(string(b))), secrets.SourceOperator, nil
	default:
		s, err := promptSecret(env, fmt.Sprintf("value for %s (not echoed): ", ref))
		if err != nil {
			return secrets.Value{}, "", fmt.Errorf("secret set: %w", err)
		}
		return secrets.New(s), secrets.SourceOperator, nil
	}
}

// trimOneTrailingEOL removes at most one trailing line ending, CRLF included —
// the newline a `printf '%s\n'` or an editor leaves on a --from-file value. It
// mirrors the store's own read-time trim so a value set from a file and one set
// from a pipe round-trip identically.
func trimOneTrailingEOL(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return s[:len(s)-2]
	}
	return strings.TrimSuffix(s, "\n")
}

// writeSecret stores the value and its source sidecar, audits the change with
// the scope/name/source (never the value), and prints length + fingerprint +
// the effect line. It is the shared body of `set` and `rotate`.
func writeSecret(ctx context.Context, env *Env, store *secrets.Store, ref secrets.Ref, value secrets.Value, source secrets.Source, action audit.Action) error {
	if err := store.Write(ref, value); err != nil {
		return err
	}
	if err := store.WriteSource(ref, source); err != nil {
		return err
	}

	ev := audit.Event{
		Actor:    audit.RootCLI(),
		Action:   action,
		Instance: instanceOfRef(ref),
		Params:   audit.Params{"scope": string(ref.Scope), "name": ref.Name, "source": string(source)},
	}
	_ = env.Host.audit().Do(ctx, ev, func(context.Context) (audit.Params, error) { return ev.Params, nil })

	verb := "set"
	if action == audit.SecretRotate {
		verb = "rotated"
	}
	fmt.Fprintf(env.Stdout, "%s %s: %d bytes, fingerprint %s (source: %s)\n",
		verb, ref, value.Len(), secrets.Fingerprint(value), source)
	fmt.Fprintln(env.Stdout, effectHint(ref))
	if !secrets.OwnershipEnforced {
		fmt.Fprintln(env.Stderr, "note: this build cannot make the file root-owned (not Linux); the value was written at the store's path.")
	}
	return nil
}

// instanceOfRef is the audit Instance field: the instance name for an instance
// secret, empty for a host one (a host secret belongs to no instance).
func instanceOfRef(ref secrets.Ref) string {
	if ref.Scope == secrets.ScopeInstance {
		return ref.Key
	}
	return ""
}

func runSecretRotate(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secret rotate", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	host := fs.Bool("host", false, "(refused) host secrets are rotated by setting a new value from the provider")
	length := fs.Int("length", secrets.DefaultPasswordLen, "length of the new password")
	restart := fs.Bool("restart", false, "restart the instance through the ladder after rotating")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	if *host {
		return usageErr(&userError{Msg: "rotate does not generate a host secret",
			Hint: "a host API key comes from its provider; set the new one with `gvm secret set --host`"})
	}

	ref, err := secretRef(false, pos)
	if err != nil {
		return err
	}

	store, err := secretStoreFor(ctx, env)
	if err != nil {
		return err
	}

	// Rotate replaces an existing value; it is not the verb for creating one.
	has, err := store.Has(ref)
	if err != nil {
		return err
	}
	if !has {
		return usageErr(&userError{Msg: fmt.Sprintf("%s is not set", ref),
			Hint: "create it first: sudo gvm secret set " + setArgs(ref) + " --generate"})
	}

	// Rule 11: gvm never rotates an adopted instance's credential — its value
	// is the live server's own, imported by cutover, and players depend on it.
	if src, ok, serr := store.Source(ref); serr == nil && ok && src == secrets.SourceAdopted {
		return refused(fmt.Errorf("%s was imported from the live server (source: adopted); "+
			"gvm never rotates an adopted credential", ref))
	}

	value, gerr := secrets.GeneratePassword(*length)
	if gerr != nil {
		return usageErr(gerr)
	}
	if err := writeSecret(ctx, env, store, ref, value, secrets.SourceGenerated, audit.SecretRotate); err != nil {
		return err
	}
	if *restart {
		return runRestart(ctx, env, []string{ref.Key})
	}
	return nil
}

// setArgs is the positional part of a `secret set` line for ref, for a hint.
func setArgs(ref secrets.Ref) string {
	if ref.Scope == secrets.ScopeHost {
		return "--host " + ref.Name
	}
	return ref.Key + " " + ref.Name
}

func runSecretList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secret list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	host := fs.Bool("host", false, "list the box-wide secrets rather than one instance's")
	asJSON := fs.Bool("json", false, "print the listing as JSON")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}

	scope := secrets.ScopeInstance
	key := ""
	if *host {
		scope, key = secrets.ScopeHost, secrets.HostKey
		if len(pos) != 0 {
			return usageErr(&userError{Msg: "--host lists every host secret", Hint: "give no instance name with --host"})
		}
	} else {
		name, nerr := oneInstance(pos)
		if nerr != nil {
			return nerr
		}
		key = name
	}

	store, err := secretStoreFor(ctx, env)
	if err != nil {
		return err
	}

	refs, err := store.List(scope, key)
	if err != nil {
		return err
	}

	type row struct {
		Name        string `json:"name"`
		Bytes       int    `json:"bytes"`
		Fingerprint string `json:"fingerprint"`
		Source      string `json:"source"`
	}
	rows := make([]row, 0, len(refs))
	for _, ref := range refs {
		r := row{Name: ref.Name, Source: "unknown"}
		if v, rerr := store.Read(ref); rerr == nil {
			r.Bytes = v.Len()
			r.Fingerprint = secrets.Fingerprint(v)
		} else {
			r.Fingerprint = "unreadable"
		}
		if src, ok, serr := store.Source(ref); serr == nil && ok {
			r.Source = string(src)
		}
		rows = append(rows, r)
	}

	if *asJSON {
		return writeJSON(env.Stdout, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(env.Stdout, "no secrets set")
		return nil
	}
	fmt.Fprintf(env.Stdout, "%-20s %-6s %-14s %s\n", "NAME", "BYTES", "FINGERPRINT", "SOURCE")
	for _, r := range rows {
		fmt.Fprintf(env.Stdout, "%-20s %-6d %-14s %s\n", r.Name, r.Bytes, r.Fingerprint, r.Source)
	}
	return nil
}

func runSecretShow(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secret show", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	host := fs.Bool("host", false, "read a box-wide secret rather than one instance's")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}

	ref, err := secretRef(*host, pos)
	if err != nil {
		return err
	}

	// The value goes to stdout in cleartext, so stdout must be a terminal a
	// person is reading — never a pipe or a file, where a redirected
	// `secret show … > x` would be a plaintext credential on disk that no later
	// tool knows to protect. (The player-facing-only default and --admin-secrets
	// of Phase 29 item 6 need the declarative `secrets:` block, which is not in
	// this build; until then any named secret is shown to root at a terminal.)
	if !stdoutIsTerminal(env) {
		return refused(&userError{Msg: "refusing to print a secret to a pipe or a file",
			Hint: "run `gvm secret show` at an interactive terminal; use `secret show-path` for the file"})
	}

	store, err := secretStoreFor(ctx, env)
	if err != nil {
		return err
	}

	value, err := store.Read(ref)
	if err != nil {
		return err
	}

	ev := audit.Event{
		Actor:    audit.RootCLI(),
		Action:   audit.SecretRead,
		Instance: instanceOfRef(ref),
		Params:   audit.Params{"scope": string(ref.Scope), "name": ref.Name},
	}
	_ = env.Host.audit().Do(ctx, ev, func(context.Context) (audit.Params, error) { return ev.Params, nil })

	fmt.Fprintln(env.Stdout, value.Reveal())
	return nil
}

func runSecretShowPath(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secret show-path", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	host := fs.Bool("host", false, "the path of a box-wide secret rather than one instance's")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}

	ref, err := secretRef(*host, pos)
	if err != nil {
		return err
	}

	store, err := secretStoreFor(ctx, env)
	if err != nil {
		return err
	}

	// The real file location, which may not exist yet: show-path answers "where
	// do I put this" as much as "where is this".
	p, err := store.Path(ref)
	if err != nil {
		return usageErr(err)
	}
	fmt.Fprintln(env.Stdout, p)
	return nil
}

// secretStoreFor is secretHost plus the store, the shape every verb but set
// uses (set threads the value through first). It keeps the resolve-root-then-
// store sequence in one place.
func secretStoreFor(ctx context.Context, env *Env) (*secrets.Store, error) {
	host, err := secretHost(ctx, env)
	if err != nil {
		return nil, err
	}
	return host.secretStore(), nil
}
