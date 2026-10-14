package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

const reapStaleRunsSQL = `
UPDATE job_runs
   SET state = 'failed', finished_at = now(),
       error = 'daemon or host restarted while this run was in flight'
 WHERE state IN ('queued','running')
   AND boot_id IS DISTINCT FROM $1::text`

// ReapStaleRuns marks any job_run that was queued or running but whose
// boot_id differs from bootID as failed. This is called once when the pool
// becomes ready.
//
// Returns the number of rows updated.
func ReapStaleRuns(ctx context.Context, pool *pgxpool.Pool, bootID string) (int64, error) {
	tag, err := pool.Exec(ctx, reapStaleRunsSQL, bootID)
	if err != nil {
		return 0, fmt.Errorf("store: reap stale runs: %w", err)
	}
	return tag.RowsAffected(), nil
}
