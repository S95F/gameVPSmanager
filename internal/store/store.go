// Package store manages the gvm Postgres connection pool, schema migrations,
// and the handful of direct SQL helpers that the daemon and CLI need.
//
// # Connection model
//
// store.Open returns immediately with a *Store whose pool starts nil. A
// background goroutine probes the Postgres socket with exponential back-off
// (1 s → 60 s ± 20 % jitter). When the probe succeeds the pool becomes live
// and the Ready() channel closes. Pool() returns nil until then, so callers
// that cannot proceed without the database must select on Ready() first.
//
// This design is deliberate: gvmd and the CLI must not block at startup
// waiting for Postgres. Game servers must run regardless of database state
// (D-14), and a daemon whose Start() hangs until the database wakes up would
// violate that requirement.
//
// # Migrations
//
// Migrations are embedded SQL run by goose and NEVER run automatically.
// Only `gvm db migrate` runs them. gvmd calls AssertCompatible at start and
// refuses to serve API requests if the schema is wrong, but it does not
// migrate.
//
// # Role discipline
//
// The pool opened here connects as gvm_ops. Web-tier code connects as gvm_web
// (column-restricted; cannot read totp_secret_ct etc.). Neither role can
// truncate tables; audit_log is append-only by grant.
package store

import (
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store holds the connection pool and the background prober state.
//
// Obtain one via Open. The zero value is invalid.
type Store struct {
	pool  *pgxpool.Pool
	ready chan struct{}
	stop  chan struct{}
	done  chan struct{}
}

// Pool returns the underlying connection pool, or nil if the store is not yet
// ready. Callers that need the pool must either check for nil or wait on
// Ready().
func (s *Store) Pool() *pgxpool.Pool {
	select {
	case <-s.ready:
		return s.pool
	default:
		return nil
	}
}

// Ready returns a channel that is closed once the pool has been confirmed
// healthy. Receiving from it blocks until the pool is ready.
func (s *Store) Ready() <-chan struct{} { return s.ready }

// Close stops the background prober and closes the pool, if open.
//
// Subsequent calls to Pool return nil.
func (s *Store) Close() {
	close(s.stop)
	<-s.done
	if s.pool != nil {
		s.pool.Close()
	}
}
