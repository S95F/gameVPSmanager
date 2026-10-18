// `gvm token` — minimal API tokens for /api/v1 clients (Phase 48 item 10).
// Root-CLI only, over peer-auth Postgres as gvm_ops; the plaintext is shown
// once and never stored (api_tokens holds the SHA-256 and a display prefix).

package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/authz"
	"github.com/S95F/gameVPSmanager/internal/identity"
	"github.com/S95F/gameVPSmanager/internal/store/dbgen/opsq"
)

func tokenGroup() subGroup {
	return subGroup{Verb: "token", Subs: []subcmd{
		{Name: "create", Summary: "mint an API token, shown once", Run: runTokenCreate,
			Examples: []string{"sudo gvm token create --user you@example.com --name ci-bot --role operator --expires 90d"}},
		{Name: "list", Summary: "list API tokens", Run: runTokenList},
		{Name: "revoke", Summary: "revoke an API token by name or prefix", Run: runTokenRevoke},
	}}
}

func runToken(ctx context.Context, env *Env, args []string) error {
	return tokenGroup().dispatch(ctx, env, args)
}

// tokenPool is userPool with the verb spelled right in refusals.
func tokenPool(ctx context.Context, env *Env, dsn string) (poolCloser, error) {
	t, err := Resolve(env, "token", "")
	if err != nil {
		return poolCloser{}, err
	}
	if err := t.RequireLocal(); err != nil {
		return poolCloser{}, err
	}
	p, closer, err := dbWaitPool(ctx, dsn)
	if err != nil {
		return poolCloser{}, fmt.Errorf("API tokens are stored in Postgres: %w", err)
	}
	return poolCloser{p, closer}, nil
}

func runTokenCreate(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	email := fs.String("user", "", "the account the token acts as (required)")
	name := fs.String("name", "", "token name, for listings and revocation (required)")
	role := fs.String("role", "viewer", "role ceiling: viewer|operator|admin — caps the token whatever the user may do")
	expires := fs.String("expires", "90d", "lifetime: a duration like 90d, or never")
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if *email == "" || *name == "" {
		return usageErr(&userError{Msg: "create needs --user and --name",
			Hint: "whose authority the token carries, and what to call it"})
	}
	ceiling, err := authz.ParseRole(*role)
	if err != nil || ceiling == authz.RoleOwner {
		return usageErr(&userError{Msg: fmt.Sprintf("%q is not a token ceiling", *role),
			Hint: "viewer, operator or admin; no token carries owner"})
	}
	var expiresAt *time.Time
	if *expires != "never" {
		d, err := parseDays(*expires)
		if err != nil {
			return usageErr(&userError{Msg: fmt.Sprintf("%q is not a lifetime", *expires),
				Hint: `a day count like 90d, or "never"`})
		}
		at := time.Now().Add(d)
		expiresAt = &at
	}

	pc, err := tokenPool(ctx, env, *dsn)
	if err != nil {
		return err
	}
	defer pc.close()

	parts, err := identity.NewAPIToken()
	if err != nil {
		return err
	}
	id, err := identity.UUIDv7String()
	if err != nil {
		return err
	}
	if err := opsq.CreateAPIToken(ctx, pc.pool, id, *email, *name, parts.Prefix,
		parts.SHA256, string(ceiling), expiresAt); err != nil {
		return fmt.Errorf("token create: %w", err)
	}

	// Shown once, in a box, and never stored: there is nothing to re-print.
	line := strings.Repeat("─", len(parts.Plaintext)+4)
	fmt.Fprintf(env.Stdout, "token %q for %s (ceiling %s", *name, *email, ceiling)
	if expiresAt != nil {
		fmt.Fprintf(env.Stdout, ", expires %s", expiresAt.Format("2006-01-02"))
	}
	fmt.Fprint(env.Stdout, ") — shown ONCE:\n")
	fmt.Fprintf(env.Stdout, "┌%s┐\n│  %s  │\n└%s┘\n", line, parts.Plaintext, line)
	fmt.Fprintf(env.Stdout, "use it as:  Authorization: Bearer %s…  (the prefix identifies it in listings)\n", parts.Prefix)
	return nil
}

// parseDays accepts the one duration shape the flag documents: "<n>d".
func parseDays(s string) (time.Duration, error) {
	num, ok := strings.CutSuffix(s, "d")
	if !ok {
		return 0, fmt.Errorf("want <n>d")
	}
	var days int
	if _, err := fmt.Sscanf(num, "%d", &days); err != nil || days <= 0 {
		return 0, fmt.Errorf("want a positive day count")
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

func runTokenList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	asJSON := fs.Bool("json", false, "print the tokens as JSON")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	pc, err := tokenPool(ctx, env, *dsn)
	if err != nil {
		return err
	}
	defer pc.close()

	tokens, err := opsq.ListAPITokens(ctx, pc.pool)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.Stdout, tokens)
	}
	if len(tokens) == 0 {
		fmt.Fprintln(env.Stdout, "no API tokens")
		return nil
	}
	fmt.Fprintf(env.Stdout, "%-16s %-10s %-32s %-8s %s\n", "NAME", "PREFIX", "USER", "CEILING", "STATE")
	for _, t := range tokens {
		state := "live"
		switch {
		case t.RevokedAt != nil:
			state = "revoked"
		case t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()):
			state = "expired"
		}
		fmt.Fprintf(env.Stdout, "%-16s %-10s %-32s %-8s %s\n", t.Name, t.Prefix, t.Email, t.RoleCeiling, state)
	}
	return nil
}

func runTokenRevoke(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() < 1 {
		return usageErr(&userError{Msg: "revoke needs a token name or prefix", Hint: "`gvm token list` shows both"})
	}
	pc, err := tokenPool(ctx, env, *dsn)
	if err != nil {
		return err
	}
	defer pc.close()

	tok, err := opsq.RevokeAPIToken(ctx, pc.pool, fs.Arg(0))
	if err != nil {
		return fmt.Errorf("token revoke: %w", err)
	}
	fmt.Fprintf(env.Stdout, "revoked %q (%s, %s)\n", tok.Name, tok.Prefix, tok.Email)
	return nil
}
