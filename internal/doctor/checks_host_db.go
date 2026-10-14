package doctor

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/store"
)

// PostgresSocketPath is the well-known Unix socket that gvm uses.
const PostgresSocketPath = "/var/run/postgresql/.s.PGSQL.5432"

// DBHostChecks returns the HOST-001..005, HOST-011, HOST-012 checks.
// All DB-dependent checks share a lazy probe that opens one connection on
// first use and caches it. If the socket does not exist or the connection
// fails, the DB checks return Skip gracefully.
func DBHostChecks() []lifecycle.DoctorCheck {
	p := &lazyPool{}
	return []lifecycle.DoctorCheck{
		{
			ID:      "HOST-001",
			Summary: "Postgres socket exists at " + PostgresSocketPath,
			Run: func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
				return checkPostgresSocket()
			},
		},
		{
			ID:      "HOST-002",
			Summary: "can connect to Postgres as gvm_ops (peer auth)",
			Run: func(ctx context.Context, env lifecycle.CheckEnv) lifecycle.CheckFinding {
				return checkPostgresConnect(ctx, p)
			},
		},
		{
			ID:      "HOST-003",
			Summary: fmt.Sprintf("schema version matches binary expectation (%d)", store.SchemaVersion),
			Run: func(ctx context.Context, env lifecycle.CheckEnv) lifecycle.CheckFinding {
				return checkSchemaVersion(ctx, p)
			},
		},
		{
			ID:      "HOST-004",
			Summary: "gvm_migrate role exists",
			Run: func(ctx context.Context, env lifecycle.CheckEnv) lifecycle.CheckFinding {
				return checkRoleExists(ctx, p, "HOST-004", "gvm_migrate")
			},
		},
		{
			ID:      "HOST-005",
			Summary: "gvm_web role exists",
			Run: func(ctx context.Context, env lifecycle.CheckEnv) lifecycle.CheckFinding {
				return checkRoleExists(ctx, p, "HOST-005", "gvm_web")
			},
		},
		{
			ID:      "HOST-011",
			Summary: "no zombie 'running' job_runs from a prior boot",
			Run: func(ctx context.Context, env lifecycle.CheckEnv) lifecycle.CheckFinding {
				return checkZombieRuns(ctx, p)
			},
		},
		{
			ID:      "HOST-012",
			Summary: "no 'left_stopped' job_runs in the last 24 h",
			Run: func(ctx context.Context, env lifecycle.CheckEnv) lifecycle.CheckFinding {
				return checkLeftStopped(ctx, p)
			},
		},
	}
}

// lazyPool opens a gvm_ops connection on first call and caches it.
type lazyPool struct {
	once sync.Once
	pool *pgxpool.Pool
	err  error
}

func (p *lazyPool) get(ctx context.Context) (*pgxpool.Pool, error) {
	p.once.Do(func() {
		if _, err := os.Stat(PostgresSocketPath); err != nil {
			p.err = fmt.Errorf("postgres socket %s not found", PostgresSocketPath)
			return
		}
		s, err := store.Open(ctx, "") // defaultDSN: gvm_ops peer auth
		if err != nil {
			p.err = fmt.Errorf("store.Open: %w", err)
			return
		}
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		select {
		case <-s.Ready():
			p.pool = s.Pool()
		case <-waitCtx.Done():
			s.Close()
			p.err = fmt.Errorf("timed out connecting to Postgres")
		}
	})
	return p.pool, p.err
}

func checkPostgresSocket() lifecycle.CheckFinding {
	if _, err := os.Stat(PostgresSocketPath); err != nil {
		if os.IsNotExist(err) {
			return lifecycle.CheckFinding{
				ID:       "HOST-001",
				Severity: lifecycle.CheckProblem,
				Message:  "Postgres socket not found: " + PostgresSocketPath,
				Fix:      "ensure postgresql is installed and running",
			}
		}
		return lifecycle.CheckFinding{
			ID:       "HOST-001",
			Severity: lifecycle.CheckWarning,
			Message:  fmt.Sprintf("cannot stat %s: %v", PostgresSocketPath, err),
		}
	}
	return lifecycle.CheckFinding{
		ID:       "HOST-001",
		Severity: lifecycle.CheckOK,
		Message:  "Postgres socket present at " + PostgresSocketPath,
	}
}

func checkPostgresConnect(ctx context.Context, p *lazyPool) lifecycle.CheckFinding {
	pool, err := p.get(ctx)
	if err != nil {
		return lifecycle.CheckFinding{
			ID:       "HOST-002",
			Severity: lifecycle.CheckWarning,
			Message:  "cannot connect as gvm_ops: " + err.Error(),
			Fix:      "ensure gvm database and gvm_ops peer-auth role exist",
		}
	}
	if pool == nil {
		return lifecycle.CheckFinding{
			ID:       "HOST-002",
			Severity: lifecycle.CheckSkip,
			Message:  "pool not available",
		}
	}
	return lifecycle.CheckFinding{
		ID:       "HOST-002",
		Severity: lifecycle.CheckOK,
		Message:  "connected to Postgres as gvm_ops",
	}
}

func checkSchemaVersion(ctx context.Context, p *lazyPool) lifecycle.CheckFinding {
	pool, err := p.get(ctx)
	if err != nil || pool == nil {
		return lifecycle.CheckFinding{
			ID:       "HOST-003",
			Severity: lifecycle.CheckSkip,
			Message:  "Postgres not reachable",
		}
	}
	m := store.NewMigrator(pool)
	v, err := m.Version(ctx)
	if err != nil {
		return lifecycle.CheckFinding{
			ID:       "HOST-003",
			Severity: lifecycle.CheckWarning,
			Message:  fmt.Sprintf("cannot read schema version: %v", err),
			Fix:      "run `gvm db migrate` to initialise the schema",
		}
	}
	want := int64(store.SchemaVersion)
	if v == want {
		return lifecycle.CheckFinding{
			ID:       "HOST-003",
			Severity: lifecycle.CheckOK,
			Message:  fmt.Sprintf("schema version %d (matches binary)", v),
		}
	}
	if v < want {
		return lifecycle.CheckFinding{
			ID:       "HOST-003",
			Severity: lifecycle.CheckProblem,
			Message:  fmt.Sprintf("schema version %d, binary expects %d — needs migration", v, want),
			Fix:      "run `gvm db migrate`",
		}
	}
	return lifecycle.CheckFinding{
		ID:       "HOST-003",
		Severity: lifecycle.CheckWarning,
		Message:  fmt.Sprintf("schema version %d is ahead of binary expectation %d — binary may be stale", v, want),
	}
}

func checkRoleExists(ctx context.Context, p *lazyPool, id, role string) lifecycle.CheckFinding {
	pool, err := p.get(ctx)
	if err != nil || pool == nil {
		return lifecycle.CheckFinding{
			ID:       id,
			Severity: lifecycle.CheckSkip,
			Message:  "Postgres not reachable",
		}
	}
	var exists bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists)
	if err != nil {
		return lifecycle.CheckFinding{
			ID:       id,
			Severity: lifecycle.CheckWarning,
			Message:  fmt.Sprintf("cannot query pg_roles: %v", err),
		}
	}
	if !exists {
		return lifecycle.CheckFinding{
			ID:       id,
			Severity: lifecycle.CheckProblem,
			Message:  fmt.Sprintf("Postgres role %q not found", role),
			Fix:      fmt.Sprintf("create role: CREATE ROLE %s NOLOGIN;", role),
		}
	}
	return lifecycle.CheckFinding{
		ID:       id,
		Severity: lifecycle.CheckOK,
		Message:  fmt.Sprintf("Postgres role %q exists", role),
	}
}

func checkZombieRuns(ctx context.Context, p *lazyPool) lifecycle.CheckFinding {
	pool, err := p.get(ctx)
	if err != nil || pool == nil {
		return lifecycle.CheckFinding{
			ID:       "HOST-011",
			Severity: lifecycle.CheckSkip,
			Message:  "Postgres not reachable",
		}
	}
	var n int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM job_runs WHERE state = 'running'`).Scan(&n)
	if err != nil {
		return lifecycle.CheckFinding{
			ID:       "HOST-011",
			Severity: lifecycle.CheckSkip,
			Message:  fmt.Sprintf("cannot query job_runs: %v", err),
		}
	}
	if n == 0 {
		return lifecycle.CheckFinding{
			ID:       "HOST-011",
			Severity: lifecycle.CheckOK,
			Message:  "no zombie 'running' job_runs",
		}
	}
	return lifecycle.CheckFinding{
		ID:       "HOST-011",
		Severity: lifecycle.CheckNote,
		Message:  fmt.Sprintf("%d job_run(s) still in 'running' state (may be from prior boot)", n),
		Fix:      "run `gvm db migrate` after a fresh start to reap stale runs",
	}
}

func checkLeftStopped(ctx context.Context, p *lazyPool) lifecycle.CheckFinding {
	pool, err := p.get(ctx)
	if err != nil || pool == nil {
		return lifecycle.CheckFinding{
			ID:       "HOST-012",
			Severity: lifecycle.CheckSkip,
			Message:  "Postgres not reachable",
		}
	}
	var n int
	cutoff := time.Now().Add(-24 * time.Hour)
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM job_runs WHERE left_stopped AND started_at >= $1`, cutoff).Scan(&n)
	if err != nil {
		return lifecycle.CheckFinding{
			ID:       "HOST-012",
			Severity: lifecycle.CheckSkip,
			Message:  fmt.Sprintf("cannot query job_runs: %v", err),
		}
	}
	if n == 0 {
		return lifecycle.CheckFinding{
			ID:       "HOST-012",
			Severity: lifecycle.CheckOK,
			Message:  "no left_stopped job_runs in the last 24 h",
		}
	}
	return lifecycle.CheckFinding{
		ID:       "HOST-012",
		Severity: lifecycle.CheckWarning,
		Message:  fmt.Sprintf("%d job_run(s) left_stopped in the last 24 h", n),
		Fix:      "investigate: `gvm job list` and check logs for stalled jobs",
	}
}
