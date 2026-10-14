package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

const mirrorInstancesSQL = `
UPDATE instances SET absent_since = now()
 WHERE deleted_at IS NULL AND absent_since IS NULL
   AND NOT (name = ANY($1::text[]))`

// MirrorInstances marks instances that are not in present as absent by
// setting absent_since. It never deletes rows. Returns the number of rows
// updated.
func MirrorInstances(ctx context.Context, pool *pgxpool.Pool, present []string) (int64, error) {
	tag, err := pool.Exec(ctx, mirrorInstancesSQL, present)
	if err != nil {
		return 0, fmt.Errorf("store: mirror instances: %w", err)
	}
	return tag.RowsAffected(), nil
}
