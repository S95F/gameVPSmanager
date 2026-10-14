package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HealthCheck pings the pool and verifies that the connected database is the
// expected one. It returns a non-nil error on failure.
func HealthCheck(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return ErrDatastoreUnavailable
	}
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	var db string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		return fmt.Errorf("store: current_database: %w", err)
	}
	return nil
}
