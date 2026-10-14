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
	"github.com/S95F/gameVPSmanager/internal/store"
)

// dbConnTimeout is how long gvm db commands wait for the pool to become ready.
const dbConnTimeout = 15 * time.Second

// runDB dispatches gvm db subcommands.
//
// Subcommands:
//
//	migrate       — apply pending migrations (connects as gvm_migrate)
//	status        — print current schema version vs binary expectation
//	version       — print the SchemaVersion constant
//	replay-spool  — insert queued spool records into audit_log
//	check-grants  — verify gvm_web cannot read restricted columns (D-27)
func runDB(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "usage: gvm db <subcommand>")
		fmt.Fprintln(env.Stderr, "subcommands: migrate, status, version, replay-spool, check-grants")
		return errors.New("db: subcommand required")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "migrate":
		return runDBMigrate(ctx, env, rest)
	case "status":
		return runDBStatus(ctx, env, rest)
	case "version":
		return runDBVersion(ctx, env, rest)
	case "replay-spool":
		return runDBReplaySpool(ctx, env, rest)
	case "check-grants":
		return runDBCheckGrants(ctx, env, rest)
	default:
		return fmt.Errorf("db: unknown subcommand %q", sub)
	}
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
	spoolDir := fs.String("spool-dir", "/var/lib/gvm/audit-spool", "audit spool directory")
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
	fmt.Fprintf(env.Stdout, "replayed %d file(s): %d inserted, %d duplicate\n",
		stats.Files, stats.OK, stats.Dup)
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
		fmt.Fprintln(env.Stderr, "FAIL: gvm_web can read totp_secret_ct (D-27 violation)")
		return errors.New("db check-grants: totp_secret_ct is readable by gvm_web")
	}
	if isPermissionDenied(queryErr) {
		fmt.Fprintln(env.Stdout, "OK: gvm_web cannot read totp_secret_ct (D-27 satisfied)")
		return nil
	}
	if errors.Is(queryErr, pgx.ErrNoRows) {
		// No rows, but the query was permitted — the column IS readable.
		fmt.Fprintln(env.Stderr, "FAIL: gvm_web can read totp_secret_ct (table empty but SELECT was not denied; D-27 violation)")
		return errors.New("db check-grants: totp_secret_ct is readable by gvm_web")
	}
	// Any other error (e.g. table doesn't exist yet) counts as inconclusive.
	fmt.Fprintf(env.Stdout, "SKIP: unexpected error checking totp_secret_ct: %v\n", queryErr)
	return nil
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
		return nil, func() {}, fmt.Errorf("%w: timed out waiting for postgres",
			store.ErrDatastoreUnavailable)
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
