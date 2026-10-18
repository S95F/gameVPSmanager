// `gvm user` — dashboard accounts, from the root CLI, in-process over
// peer-auth Postgres as gvm_ops (Phase 48 item 1). Never through admin.sock:
// the CLI is already root, and rule 3 keeps the socket out of its hands.
//
// Root never chooses a friend's password: `create` reads the FIRST account's
// password from the terminal (the person at the keyboard is the owner being
// created), and everything after that is `reset`, which clears factors and
// revokes sessions so the user re-enrols themselves.

package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/S95F/gameVPSmanager/internal/authz"
	"github.com/S95F/gameVPSmanager/internal/avail"
	"github.com/S95F/gameVPSmanager/internal/identity"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/secrets"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/internal/store/dbgen/opsq"
)

// userGroup is the `gvm user` subcommand table.
func userGroup() subGroup {
	return subGroup{Verb: "user", Subs: []subcmd{
		{Name: "create", Summary: "create an owner or admin account (password + TOTP at the terminal)", Run: runUserCreate,
			Examples: []string{"sudo gvm user create --email you@example.com --role owner"}},
		{Name: "invite", Summary: "invite an operator or viewer by single-use URL", Run: runUserInvite},
		{Name: "list", Summary: "list dashboard users", Run: runUserList},
		{Name: "show", Summary: "show one account and its grants", Run: runUserShow},
		{Name: "roles", Summary: "print what each role may do", Run: runUserRoles},
		{Name: "grant", Summary: "give a user a role on one instance", Run: runUserGrant,
			Examples: []string{"sudo gvm user grant alice@example.com pz1 operator"}},
		{Name: "revoke-grant", Summary: "remove a user's grant on one instance", Run: runUserRevokeGrant},
		{Name: "disable", Summary: "disable an account and revoke its sessions and tokens", Run: runUserDisable},
		{Name: "enable", Summary: "re-enable a disabled account", Run: runUserEnable},
		{Name: "reset", Summary: "clear factors and revoke sessions, so the user re-enrols", Run: runUserReset,
			Examples: []string{"sudo gvm user reset alice@example.com --totp"}},
		{Name: "sessions", Summary: "list live dashboard sessions", Run: runUserSessions},
		{Name: "revoke-sessions", Summary: "revoke a user's live sessions", Run: runUserRevokeSessions},
	}}
}

func runUser(ctx context.Context, env *Env, args []string) error {
	return userGroup().dispatch(ctx, env, args)
}

// userPool resolves the transport (root, in-process) and opens the gvm_ops
// pool, failing fast with the one honest sentence when Postgres is down.
func userPool(ctx context.Context, env *Env, dsn string) (pool poolCloser, err error) {
	t, err := Resolve(env, "user", "")
	if err != nil {
		return poolCloser{}, err
	}
	if err := t.RequireLocal(); err != nil {
		return poolCloser{}, err
	}
	p, closer, err := dbWaitPool(ctx, dsn)
	if err != nil {
		return poolCloser{}, fmt.Errorf("dashboard users are stored in Postgres: %w", err)
	}
	return poolCloser{p, closer}, nil
}

func runUserCreate(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	email := fs.String("email", "", "the account's email address (required)")
	role := fs.String("role", "", "global role: owner or admin (required)")
	display := fs.String("display-name", "", "name shown in the dashboard (default: the email)")
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if *email == "" {
		return usageErr(&userError{Msg: "create needs --email", Hint: "the new account's email address"})
	}
	switch *role {
	case "owner", "admin":
	case "":
		return usageErr(&userError{Msg: "create needs --role owner or --role admin",
			Hint: "operators and viewers are invited (`gvm user invite`), never created by root"})
	default:
		return usageErr(&userError{Msg: fmt.Sprintf("%q is not a role create accepts", *role),
			Hint: "owner or admin; operators and viewers enrol through an invite"})
	}

	// The master key seals the TOTP secret; without it enrolment cannot be
	// stored, so refuse before asking anyone to type anything.
	key, err := secrets.LoadMasterKey(secrets.MasterKeyPath)
	if err != nil {
		return fmt.Errorf("user create: %w\n  run: sudo gvm host init --dashboard", err)
	}
	box, err := identity.NewBox([]byte(key.Value.Reveal()))
	if err != nil {
		return fmt.Errorf("user create: master key: %w", err)
	}

	pc, err := userPool(ctx, env, *dsn)
	if err != nil {
		return err
	}
	defer pc.close()

	// Password, at the terminal, hashed before anything is stored.
	password, err := promptSecret(env, fmt.Sprintf("Password for %s (min %d characters): ", *email, identity.MinPasswordRunes))
	if err != nil {
		return err
	}
	if isTerminal(env.Stdin) {
		again, err := promptSecret(env, "Repeat password: ")
		if err != nil {
			return err
		}
		if again != password {
			return fmt.Errorf("user create: the passwords do not match; nothing was created")
		}
	}
	hash, err := identity.HashPassword(password)
	if err != nil {
		return fmt.Errorf("user create: %w", err)
	}

	// TOTP enrolment is mandatory for owner and admin: generate, show, and
	// require one valid code before anything lands in the database.
	totpSecret, err := identity.NewTOTPSecret()
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "\nAdd this account to your authenticator app:\n")
	fmt.Fprintf(env.Stdout, "  otpauth://totp/gvm:%s?secret=%s&issuer=gvm\n", *email, totpSecret)
	fmt.Fprintf(env.Stdout, "  (or enter the secret by hand: %s)\n\n", totpSecret)
	code, err := promptSecret(env, "Enter the 6-digit code it shows: ")
	if err != nil {
		return err
	}
	step, err := identity.VerifyTOTP(totpSecret, strings.TrimSpace(code), 0)
	if err != nil {
		return fmt.Errorf("user create: that code is not valid; nothing was created (check the device's clock and run create again)")
	}

	id, err := identity.UUIDv7String()
	if err != nil {
		return err
	}
	name := *display
	if name == "" {
		name = *email
	}
	if err := opsq.CreateUser(ctx, pc.pool, id, *email, name, hash, *role); err != nil {
		return fmt.Errorf("user create: %w", err)
	}
	sealed, err := box.SealTOTP(id, totpSecret)
	if err != nil {
		return fmt.Errorf("user create: seal totp: %w", err)
	}
	if err := opsq.SetTOTP(ctx, pc.pool, id, sealed, 1); err != nil {
		return fmt.Errorf("user create: %w", err)
	}
	_ = opsq.SetLastTOTPStep(ctx, pc.pool, id, step)

	fmt.Fprintf(env.Stdout, "\ncreated %s (%s), TOTP enrolled\n", *email, *role)
	fmt.Fprintf(env.Stdout, "next: sign in at the dashboard\n")
	return nil
}

func runUserInvite(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user invite", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	instName := fs.String("instance", "", "grant this instance alongside the invite")
	role := fs.String("role", "viewer", "the instance grant's role: viewer|operator")
	ttl := fs.Duration("ttl", 24*time.Hour, "how long the invite URL stays redeemable")
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() < 1 {
		return usageErr(&userError{Msg: "invite needs an email",
			Hint: "e.g. gvm user invite alice@example.com --instance pz1 --role operator"})
	}
	email := fs.Arg(0)
	switch *role {
	case "viewer", "operator":
	default:
		// An invited account has a viewer ceiling unless granted more, and a
		// grant above operator is admin's irreversible set — root-CLI only.
		return usageErr(&userError{Msg: fmt.Sprintf("%q is not an invite role", *role),
			Hint: "viewer or operator; admin and owner accounts are created with `gvm user create`"})
	}

	// The instance's spec is loaded BEFORE anything is written, so a typo'd
	// name creates no half-invited account.
	var instUID, instGame, instSpec string
	if *instName != "" {
		paths := instance.PathsFor(*instName)
		inst, diags, err := spec.LoadInstance(paths.Spec)
		if err != nil {
			return fmt.Errorf("user invite: no instance %q on this host: %w", *instName, err)
		}
		if diags.HasErrors() {
			return fmt.Errorf("user invite: %s does not validate:\n%s", paths.Spec, diags.String())
		}
		instUID, instGame, instSpec = string(inst.Metadata.UID), inst.Spec.Game, paths.Spec
	}

	pc, err := userPool(ctx, env, *dsn)
	if err != nil {
		return err
	}
	defer pc.close()

	// The account, if it does not exist yet: a viewer with no factors, which
	// can do nothing until the invite is redeemed.
	if _, err := opsq.GetUserID(ctx, pc.pool, email); err != nil {
		if !strings.Contains(err.Error(), "no user") {
			return err
		}
		id, err := identity.UUIDv7String()
		if err != nil {
			return err
		}
		if err := opsq.CreateUser(ctx, pc.pool, id, email, email, "", "viewer"); err != nil {
			return fmt.Errorf("user invite: %w", err)
		}
	}
	if *instName != "" {
		if err := opsq.UpsertInstance(ctx, pc.pool, instUID, *instName, instGame, instSpec); err != nil {
			return err
		}
		if err := opsq.SetGrant(ctx, pc.pool, email, *instName, *role); err != nil {
			return fmt.Errorf("user invite: %w", err)
		}
	}

	url, err := mintInvite(ctx, env, pc, email, *ttl)
	if err != nil {
		return fmt.Errorf("user invite: %w", err)
	}
	fmt.Fprintf(env.Stdout, "invited %s", email)
	if *instName != "" {
		fmt.Fprintf(env.Stdout, " (%s on %s)", *role, *instName)
	}
	fmt.Fprintf(env.Stdout, " — single use, valid %s:\n\n  %s\n\n", ttl.String(), url)
	fmt.Fprintln(env.Stdout, "send it over a channel you trust; whoever opens it becomes this account")
	return nil
}

// mintInvite revokes any older invites, mints a fresh single-use token and
// returns the URL to hand over. The plaintext is printed once, never stored.
func mintInvite(ctx context.Context, env *Env, pc poolCloser, email string, ttl time.Duration) (string, error) {
	if err := opsq.RevokeInvites(ctx, pc.pool, email); err != nil {
		return "", err
	}
	parts, err := identity.NewInviteToken()
	if err != nil {
		return "", err
	}
	id, err := identity.UUIDv7String()
	if err != nil {
		return "", err
	}
	if err := opsq.CreateInvite(ctx, pc.pool, id, email, parts.SHA256, ttl); err != nil {
		return "", err
	}
	return enrolBase(env) + "/enrol?token=" + parts.Plaintext, nil
}

// enrolBase is the dashboard's public origin from edge.site, or a bare path
// with a note when the host config does not name one.
func enrolBase(env *Env) string {
	data, err := os.ReadFile("/etc/gvm/gvm.yaml")
	if err != nil {
		return "https://<your-dashboard>"
	}
	host, diags := spec.LoadHost("/etc/gvm/gvm.yaml", data)
	if diags.HasErrors() || host.Spec.Edge.Site == "" {
		return "https://<your-dashboard>"
	}
	return "https://" + host.Spec.Edge.Site
}

func runUserList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	asJSON := fs.Bool("json", false, "print the accounts as JSON")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	pc, err := userPool(ctx, env, *dsn)
	if err != nil {
		return err
	}
	defer pc.close()

	users, err := opsq.ListUsers(ctx, pc.pool)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.Stdout, users)
	}
	if len(users) == 0 {
		fmt.Fprintln(env.Stdout, "no dashboard users; create the first owner with:")
		fmt.Fprintln(env.Stdout, "  sudo gvm user create --email you@example.com --role owner")
		return nil
	}
	fmt.Fprintf(env.Stdout, "%-32s %-8s %-6s %s\n", "EMAIL", "ROLE", "TOTP", "STATE")
	for _, u := range users {
		state := "active"
		if u.DisabledAt != nil {
			state = "disabled"
		}
		totp := "no"
		if u.TOTPEnrolled {
			totp = "yes"
		}
		fmt.Fprintf(env.Stdout, "%-32s %-8s %-6s %s\n", u.Email, u.GlobalRole, totp, state)
	}
	return nil
}

func runUserShow(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user show", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() < 1 {
		return usageErr(&userError{Msg: "show needs an email", Hint: "the account to show"})
	}
	email := fs.Arg(0)
	pc, err := userPool(ctx, env, *dsn)
	if err != nil {
		return err
	}
	defer pc.close()

	users, err := opsq.ListUsers(ctx, pc.pool)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.Email != email {
			continue
		}
		fmt.Fprintf(env.Stdout, "email        %s\n", u.Email)
		fmt.Fprintf(env.Stdout, "display name %s\n", u.DisplayName)
		fmt.Fprintf(env.Stdout, "global role  %s\n", u.GlobalRole)
		fmt.Fprintf(env.Stdout, "totp         %v\n", u.TOTPEnrolled)
		if u.LastLoginAt != nil {
			fmt.Fprintf(env.Stdout, "last login   %s\n", u.LastLoginAt.Format(time.RFC3339))
		}
		if u.DisabledAt != nil {
			fmt.Fprintf(env.Stdout, "disabled at  %s\n", u.DisabledAt.Format(time.RFC3339))
		}
		grants, err := opsq.ListGrants(ctx, pc.pool, email)
		if err != nil {
			return err
		}
		for _, g := range grants {
			fmt.Fprintf(env.Stdout, "grant        %s on %s\n", g.Role, g.Instance)
		}
		return nil
	}
	return fmt.Errorf("user show: no user %q", email)
}

// runUserRoles prints the §15.2 matrix as a plain-English table, straight
// from internal/authz so it cannot drift from what Check enforces.
func runUserRoles(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user roles", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}

	added := map[authz.Role][]string{}
	seen := map[authz.Permission]bool{}
	for _, role := range []authz.Role{authz.RoleViewer, authz.RoleOperator, authz.RoleAdmin, authz.RoleOwner} {
		for _, g := range authz.AllGrants() {
			if g.Role != role || seen[g.Permission] {
				continue
			}
			seen[g.Permission] = true
			added[role] = append(added[role], string(g.Permission))
		}
		sort.Strings(added[role])
	}

	fmt.Fprintln(env.Stdout, "Roles nest: viewer < operator < admin < owner. Each adds to the one below.")
	for _, role := range []authz.Role{authz.RoleViewer, authz.RoleOperator, authz.RoleAdmin, authz.RoleOwner} {
		fmt.Fprintf(env.Stdout, "\n%s adds:\n", role)
		for _, p := range added[role] {
			fmt.Fprintf(env.Stdout, "  %s\n", p)
		}
	}
	fmt.Fprintln(env.Stdout, "\nUser and grant management, secret rotation, manifest publishing and host config stay root-CLI only in this release, whatever the matrix allows.")
	return nil
}

func runUserGrant(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user grant", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() < 3 {
		return usageErr(&userError{Msg: "grant needs <email> <instance> <role>",
			Hint: "e.g. gvm user grant alice@example.com pz1 operator"})
	}
	email, instName, roleArg := fs.Arg(0), fs.Arg(1), fs.Arg(2)
	role, err := authz.ParseRole(roleArg)
	if err != nil {
		return usageErr(&userError{Msg: fmt.Sprintf("%q is not a role", roleArg),
			Hint: "viewer, operator, admin or owner"})
	}

	// The grant references the instance's mirror row, which may not exist
	// before gvmd's first sweep — so mirror this one instance from its spec,
	// the control plane itself (rule 1).
	paths := instance.PathsFor(instName)
	inst, diags, err := spec.LoadInstance(paths.Spec)
	if err != nil {
		return fmt.Errorf("user grant: no instance %q on this host: %w", instName, err)
	}
	if diags.HasErrors() {
		return fmt.Errorf("user grant: %s does not validate:\n%s", paths.Spec, diags.String())
	}

	pc, err := userPool(ctx, env, *dsn)
	if err != nil {
		return err
	}
	defer pc.close()

	if err := opsq.UpsertInstance(ctx, pc.pool, string(inst.Metadata.UID), instName, inst.Spec.Game, paths.Spec); err != nil {
		return err
	}
	if err := opsq.SetGrant(ctx, pc.pool, email, instName, string(role)); err != nil {
		return fmt.Errorf("user grant: %w", err)
	}
	fmt.Fprintf(env.Stdout, "granted %s on %s to %s\n", role, instName, email)
	return nil
}

func runUserRevokeGrant(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user revoke-grant", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() < 2 {
		return usageErr(&userError{Msg: "revoke-grant needs <email> <instance>", Hint: ""})
	}
	pc, err := userPool(ctx, env, *dsn)
	if err != nil {
		return err
	}
	defer pc.close()
	if err := opsq.RevokeGrant(ctx, pc.pool, fs.Arg(0), fs.Arg(1)); err != nil {
		return fmt.Errorf("user revoke-grant: %w", err)
	}
	fmt.Fprintf(env.Stdout, "revoked %s's grant on %s\n", fs.Arg(0), fs.Arg(1))
	return nil
}

func runUserDisable(ctx context.Context, env *Env, args []string) error {
	return setUserDisabled(ctx, env, args, true)
}

func runUserEnable(ctx context.Context, env *Env, args []string) error {
	return setUserDisabled(ctx, env, args, false)
}

func setUserDisabled(ctx context.Context, env *Env, args []string, disabled bool) error {
	verb := "enable"
	if disabled {
		verb = "disable"
	}
	fs := flag.NewFlagSet("user "+verb, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() < 1 {
		return usageErr(&userError{Msg: verb + " needs an email", Hint: "the account to " + verb})
	}
	pc, err := userPool(ctx, env, *dsn)
	if err != nil {
		return err
	}
	defer pc.close()
	if err := opsq.SetUserDisabled(ctx, pc.pool, fs.Arg(0), disabled); err != nil {
		return fmt.Errorf("user %s: %w", verb, err)
	}
	if disabled {
		fmt.Fprintf(env.Stdout, "disabled %s; live sessions and tokens are revoked\n", fs.Arg(0))
	} else {
		fmt.Fprintf(env.Stdout, "enabled %s\n", fs.Arg(0))
	}
	return nil
}

func runUserReset(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user reset", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	totp := fs.Bool("totp", false, "clear the TOTP enrolment")
	password := fs.Bool("password", false, "clear the password")
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() < 1 {
		return usageErr(&userError{Msg: "reset needs an email", Hint: "add --totp and/or --password for the factor to clear"})
	}
	if !*totp && !*password {
		return usageErr(&userError{Msg: "reset needs --totp and/or --password",
			Hint: "say which factor to clear; sessions and tokens are revoked either way"})
	}
	pc, err := userPool(ctx, env, *dsn)
	if err != nil {
		return err
	}
	defer pc.close()
	if err := opsq.ResetUserFactors(ctx, pc.pool, fs.Arg(0), *password, *totp); err != nil {
		return fmt.Errorf("user reset: %w", err)
	}
	fmt.Fprintf(env.Stdout, "reset %s: sessions and tokens revoked", fs.Arg(0))
	if *password {
		fmt.Fprint(env.Stdout, ", password cleared")
	}
	if *totp {
		fmt.Fprint(env.Stdout, ", TOTP cleared")
	}
	fmt.Fprintln(env.Stdout)

	// A fresh invite, so the user re-enrols their own factors — root never
	// chooses a friend's password (item 1).
	url, err := mintInvite(ctx, env, pc, fs.Arg(0), 24*time.Hour)
	if err != nil {
		return fmt.Errorf("user reset: factors cleared, but minting the fresh invite failed: %w", err)
	}
	fmt.Fprintf(env.Stdout, "fresh invite — single use, valid 24h:\n\n  %s\n", url)
	return nil
}

func runUserSessions(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user sessions", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	email := ""
	if fs.NArg() > 0 {
		email = fs.Arg(0)
	}
	pc, err := userPool(ctx, env, *dsn)
	if err != nil {
		return err
	}
	defer pc.close()
	sessions, err := opsq.ListLiveSessions(ctx, pc.pool, email)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Fprintln(env.Stdout, "no live sessions")
		return nil
	}
	for _, s := range sessions {
		ip := ""
		if s.IP != nil {
			ip = *s.IP
		}
		fmt.Fprintf(env.Stdout, "%-32s last seen %s  expires %s  %s\n",
			s.Email, s.LastSeenAt.Format(time.RFC3339), s.ExpiresAt.Format(time.RFC3339), ip)
	}
	return nil
}

func runUserRevokeSessions(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("user revoke-sessions", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_ops)")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() < 1 {
		return usageErr(&userError{Msg: "revoke-sessions needs an email", Hint: "whose sessions to revoke"})
	}
	pc, err := userPool(ctx, env, *dsn)
	if err != nil {
		return err
	}
	defer pc.close()
	n, err := opsq.RevokeUserSessions(ctx, pc.pool, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "revoked %d session(s) for %s\n", n, fs.Arg(0))
	return nil
}

// ── Shared helpers ───────────────────────────────────────────────────────────

// errAdminUnbuilt is the refusal the still-unbuilt admin groups share.
//
// The command is routed, so `--help` and argument validation already ran; what
// is missing is the engine behind it. It returns an avail error so Main prints
// the one standard line and exits 3 (U8), and it NEVER names a unit this build
// does not ship (U10).
func errAdminUnbuilt(op string) error {
	switch {
	case strings.HasPrefix(op, "secret"):
		return avail.New("storing and reading instance secrets arrives in a later release")
	case strings.HasPrefix(op, "audit"):
		return avail.New("reading the audit log arrives with the audit spool reader")
	}
	return avail.New("that part of gvm is not in this build")
}

// poolCloser pairs a pool with its closer so callers defer one thing.
type poolCloser struct {
	pool   *pgxpool.Pool
	closer func()
}

func (p poolCloser) close() {
	if p.closer != nil {
		p.closer()
	}
}

// isTerminal reports whether r is an interactive terminal.
func isTerminal(r any) bool {
	f, ok := r.(*os.File)
	return ok && stdinIsTerminal(f)
}

// promptSecret reads a secret: without echo at a terminal (prompt_term.go),
// as one plain line otherwise (pipes and tests). The prompt goes to stderr so
// stdout stays parseable.
func promptSecret(env *Env, prompt string) (string, error) {
	fmt.Fprint(env.Stderr, prompt)
	if f, ok := env.Stdin.(*os.File); ok {
		if v, handled, err := readPasswordTerminal(f); handled {
			fmt.Fprintln(env.Stderr)
			if err != nil {
				return "", err
			}
			return v, nil
		}
	}
	return readLine(env)
}

// readLine reads one line byte-by-byte, so consecutive prompts from one piped
// stdin each get their own line (a buffered reader would swallow the rest).
func readLine(env *Env) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := env.Stdin.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			if buf[0] != '\r' {
				b.WriteByte(buf[0])
			}
		}
		if err != nil {
			if b.Len() > 0 {
				break
			}
			return "", fmt.Errorf("reading input: %w", err)
		}
	}
	return b.String(), nil
}
