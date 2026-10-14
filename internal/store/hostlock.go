package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

const acquireHostLockSQL = `
INSERT INTO host_locks (key, holder_run_id, holder_pid, acquired_at, note)
VALUES ($1, $2, $3, now(), '')
ON CONFLICT (key) DO UPDATE
    SET holder_run_id = EXCLUDED.holder_run_id,
        holder_pid    = EXCLUDED.holder_pid,
        acquired_at   = EXCLUDED.acquired_at`

const releaseHostLockSQL = `DELETE FROM host_locks WHERE key = $1`

// AcquireHostLock upserts a host_locks row, claiming the named key for the
// given job run and PID.
func AcquireHostLock(ctx context.Context, pool *pgxpool.Pool, key string, runID int64, pid int) error {
	if _, err := pool.Exec(ctx, acquireHostLockSQL, key, runID, pid); err != nil {
		return fmt.Errorf("store: acquire host lock %q: %w", key, err)
	}
	return nil
}

// ReleaseHostLock deletes the host_locks row for key.
func ReleaseHostLock(ctx context.Context, pool *pgxpool.Pool, key string) error {
	if _, err := pool.Exec(ctx, releaseHostLockSQL, key); err != nil {
		return fmt.Errorf("store: release host lock %q: %w", key, err)
	}
	return nil
}
