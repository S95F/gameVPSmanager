package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/S95F/gameVPSmanager/internal/audit"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/store"
)

// dbConnTimeout is how long gvm db commands make their one quiet attempt to
// reach Postgres before failing with a plain sentence. It is short on purpose:
// an operator at a prompt should not wait 15 s to be told the socket is not
// answering, and nothing a db command does is load-bearing for a running world
// (failure-recovery-9). A longer wait is opt-in, for a boot-ordering race.
const dbConnTimeout = 2 * time.Second

// runDB dispatches gvm db subcommands.
//
// Subcommands:
//
//	migrate       — apply pending migrations (connects as gvm_migrate)
//	status        — print current schema version vs binary expectation
//	version       — print the SchemaVersion constant
//	replay-spool  — insert queued spool records into audit_log
//	check-grants  — verify gvm_web cannot read restricted columns
func runDB(ctx context.Context, env *Env, args []string) error {
	return subGroup{Verb: "db", Subs: []subcmd{
		{Name: "migrate", Summary: "apply pending schema migrations", Run: runDBMigrate},
		{Name: "status", Summary: "print the live schema version vs this binary", Run: runDBStatus},
		{Name: "version", Summary: "print the schema version this binary requires", Run: runDBVersion},
		{Name: "replay-spool", Summary: "insert queued spool records into the audit log", Run: runDBReplaySpool},
		{Name: "check-grants", Summary: "verify the web role cannot read restricted columns", Run: runDBCheckGrants},
	}}.dispatch(ctx, env, args)
}

func runDBVersion(_ context.Context, env *Env, _ []string) error {
	fmt.Fprintf(env.Stdout, "%d\n", store.SchemaVersion)
	return nil
}

func runDBStatus(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("db status", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	pool, closer, err := dbWaitPool(ctx, *dsn)
	if err != nil {
		return fmt.Errorf("db status: %w", err)
	}
	defer closer()

	m := store.NewMigrator(pool)
	v, err := m.Version(ctx)
	if err != nil {
		return fmt.Errorf("db status: %w", err)
	}
	fmt.Fprintf(env.Stdout, "current schema version : %d\n", v)
	fmt.Fprintf(env.Stdout, "binary expects version : %d\n", store.SchemaVersion)
	if v == int64(store.SchemaVersion) {
		fmt.Fprintln(env.Stdout, "status: up to date")
	} else if v < int64(store.SchemaVersion) {
		fmt.Fprintln(env.Stdout, "status: needs migration — run `gvm db migrate`")
	} else {
		fmt.Fprintln(env.Stdout, "status: schema is AHEAD of binary — downgrade not supported")
	}
	return nil
}

func runDBMigrate(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("db migrate", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket, gvm_migrate role)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	migDSN := *dsn
	if migDSN == "" {
		migDSN = "postgres://gvm_migrate@/gvm?host=/var/run/postgresql"
	}

	pool, closer, err := dbWaitPool(ctx, migDSN)
	if err != nil {
		return fmt.Errorf("db migrate: %w", err)
	}
	defer closer()

	m := store.NewMigrator(pool)
	before, _ := m.Version(ctx)
	if err := m.Migrate(ctx); err != nil {
		return fmt.Errorf("db migrate: %w", err)
	}
	after, _ := m.Version(ctx)
	if before == after {
		fmt.Fprintf(env.Stdout, "db: already at version %d (nothing to do)\n", after)
	} else {
		fmt.Fprintf(env.Stdout, "db: migrated %d → %d\n", before, after)
	}
	return nil
}

func runDBReplaySpool(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("db replay-spool", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth)")
	// Defaults to instance.SpoolDir, the one directory the writer, the tmpfiles
	// entry and this replayer all agree on. The old /var/lib/gvm/audit-spool
	// default read an empty directory while the spool filled elsewhere
	// (admin-security-7).
	spoolDir := fs.String("spool-dir", instance.SpoolDir, "audit spool directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	pool, closer, err := dbWaitPool(ctx, *dsn)
	if err != nil {
		return fmt.Errorf("db replay-spool: %w", err)
	}
	defer closer()

	stats, err := audit.ReplaySpool(ctx, pool, *spoolDir)
	if err != nil {
		return fmt.Errorf("db replay-spool: %w", err)
	}
	fmt.Fprintf(env.Stdout, "replayed %d file(s) from %s: %d inserted, %d duplicate\n",
		stats.Files, *spoolDir, stats.OK, stats.Dup)
	return nil
}

func runDBCheckGrants(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("db check-grants", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket, gvm_web role)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	webDSN := *dsn
	if webDSN == "" {
		webDSN = "postgres://gvm_web@/gvm?host=/var/run/postgresql"
	}

	pool, closer, err := dbWaitPool(ctx, webDSN)
	if err != nil {
		return fmt.Errorf("db check-grants: %w", err)
	}
	defer closer()

	// D-27: gvm_web must not be able to SELECT totp_secret_ct, totp_key_id,
	// or last_totp_step.
	var dummy []byte
	queryErr := pool.QueryRow(ctx, `SELECT totp_secret_ct FROM users LIMIT 1`).Scan(&dummy)
	if queryErr == nil {
		// A row was returned: SELECT was permitted and a value was readable.
		fmt.Fprintln(env.Stderr, "FAIL: gvm_web can read totp_secret_ct (a restricted column was readable)")
		return errors.New("db check-grants: totp_secret_ct is readable by gvm_web")
	}
	if isPermissionDenied(queryErr) {
		fmt.Fprintln(env.Stdout, "OK: gvm_web cannot read totp_secret_ct (the restricted column is not readable)")
		return nil
	}
	if errors.Is(queryErr, pgx.ErrNoRows) {
		// No rows, but the query was permitted — the column IS readable.
		fmt.Fprintln(env.Stderr, "FAIL: gvm_web can read totp_secret_ct (table empty but SELECT was not denied — the column is readable)")
		return errors.New("db check-grants: totp_secret_ct is readable by gvm_web")
	}
	// Any other error (e.g. table doesn't exist yet) counts as inconclusive.
	// Inconclusive (e.g. the table does not exist yet): exit 4, so a CI gate
	// does not read "could not check" as "checked and safe" (admin-security-7).
	fmt.Fprintf(env.Stdout, "INCONCLUSIVE: could not check totp_secret_ct: %v\n", queryErr)
	return checked(fmt.Errorf("db check-grants: inconclusive: %v", queryErr))
}

// dbWaitPool opens a store, waits for the pool to be ready within
// dbConnTimeout, and returns the pool and a closer that must be deferred.
func dbWaitPool(ctx context.Context, dsn string) (*pgxpool.Pool, func(), error) {
	s, err := store.Open(ctx, dsn)
	if err != nil {
		return nil, func() {}, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, dbConnTimeout)
	defer cancel()
	select {
	case <-s.Ready():
		return s.Pool(), s.Close, nil
	case <-waitCtx.Done():
		s.Close()
		return nil, func() {}, fmt.Errorf("%w: Postgres is not reachable at "+
			"/var/run/postgresql (check: systemctl status postgresql). Game servers, backups "+
			"and updates are unaffected", store.ErrDatastoreUnavailable)
	}
}

// isPermissionDenied returns true when the error is a Postgres permission-denied
// error (SQLSTATE 42501).
func isPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "42501") ||
		strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "insufficient privilege")
}
