package audit

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SpoolStats summarises one ReplaySpool run.
type SpoolStats struct {
	Files int
	OK    int
	Dup   int
}

// ReplaySpool reads every NDJSON spool file in spoolDir and inserts each
// record into audit_log using INSERT … ON CONFLICT (spool_id) DO NOTHING.
//
// It returns the total ok and dup counts across all files. A file that
// replays successfully is recorded in spool_replays. A file whose replay
// fails partway is left on disk so the next call can retry it.
//
// This function is called by `gvm db replay-spool`.
func ReplaySpool(ctx context.Context, pool *pgxpool.Pool, spoolDir string) (stats SpoolStats, err error) {
	// List spool files oldest-first. We don't have a boot-id here, so we use
	// an empty string which ListSpools treats as "no current boot".
	files, err := ListSpools(spoolDir, "")
	if err != nil {
		return stats, fmt.Errorf("audit: replay spool: list %s: %w", spoolDir, err)
	}

	sink := &pgReplaySink{pool: pool}
	for _, path := range files {
		res, ferr := Replay(ctx, path, sink)
		stats.Files++
		stats.OK += res.Records
		stats.Dup += res.Duplicate

		if ferr != nil {
			// Transient error (e.g. Postgres went away). Stop here so the
			// file is not marked replayed with missing rows.
			return stats, fmt.Errorf("audit: replay spool: %s: %w", filepath.Base(path), ferr)
		}

		// Record successful replay in spool_replays.
		if recErr := recordSpoolReplay(ctx, pool, filepath.Base(path), res.Records, res.Duplicate); recErr != nil {
			// Non-fatal: don't abort the whole replay over a bookkeeping write.
			_ = recErr
		}
	}
	return stats, nil
}

// pgReplaySink is a Sink that wraps InsertSpoolRow for use by Replay.
type pgReplaySink struct {
	pool *pgxpool.Pool
}

func (s *pgReplaySink) Write(ctx context.Context, r Record) error {
	err := InsertSpoolRow(ctx, s.pool, r)
	if errors.Is(err, ErrDuplicate) {
		return ErrDuplicate
	}
	return err
}

const recordSpoolReplaySQL = `
INSERT INTO spool_replays (file, replayed_at, rows_ok, rows_dup)
VALUES ($1, now(), $2, $3)
ON CONFLICT (file) DO UPDATE
    SET replayed_at = now(), rows_ok = EXCLUDED.rows_ok, rows_dup = EXCLUDED.rows_dup`

func recordSpoolReplay(ctx context.Context, pool *pgxpool.Pool, file string, ok, dup int) error {
	_, err := pool.Exec(ctx, recordSpoolReplaySQL, file, ok, dup)
	return err
}
