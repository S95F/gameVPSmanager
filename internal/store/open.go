package store

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultDSN is the peer-auth unix-socket DSN for the gvm_ops role.
const defaultDSN = "postgres://gvm_ops@/gvm?host=/var/run/postgresql"

// backoff parameters for the connection prober.
const (
	probeInitial = 1 * time.Second
	probeMax     = 60 * time.Second
	probeJitter  = 0.20 // ±20 %
)

// Open returns a *Store immediately. A background goroutine probes Postgres
// with exponential back-off (1 s → 60 s ± 20 % jitter) until the connection
// succeeds, at which point Ready() closes.
//
// dsn may be empty, in which case defaultDSN is used.
//
// ctx is only used for the initial pool creation attempt; subsequent probes
// use a fresh background context so that a short-lived ctx does not prevent
// reconnection.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		dsn = defaultDSN
	}
	s := &Store{
		ready: make(chan struct{}),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go s.probe(dsn)
	return s, nil
}

// probe is the background goroutine that repeatedly attempts to open and ping
// the pool. When it succeeds it sets s.pool and closes s.ready, then exits.
func (s *Store) probe(dsn string) {
	defer close(s.done)
	delay := probeInitial
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		pool, err := pgxpool.New(ctx, dsn)
		cancel()
		if err == nil {
			pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
			pingErr := pool.Ping(pctx)
			pcancel()
			if pingErr == nil {
				s.pool = pool
				close(s.ready)
				return
			}
			pool.Close()
			slog.Warn("store: pool ping failed, retrying",
				"err", pingErr, "delay", delay)
		} else {
			slog.Warn("store: pool open failed, retrying",
				"err", err, "delay", delay)
		}

		jitter := time.Duration(float64(delay) * probeJitter * (rand.Float64()*2 - 1))
		sleep := delay + jitter
		if sleep < 0 {
			sleep = probeInitial
		}

		select {
		case <-s.stop:
			return
		case <-time.After(sleep):
		}

		delay *= 2
		if delay > probeMax {
			delay = probeMax
		}
	}
}
