package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgSink is an audit.Sink that writes records to the Postgres audit_log table.
//
// It falls back silently (returns nil) when the pool is nil or unreachable,
// so the daemon's spool sink can still record the event. The caller is
// expected to wire a MultiSink{PgSink, Spool} so that Postgres failures are
// covered by the spool.
type PgSink struct {
	pool *pgxpool.Pool
}

// NewPgSink returns a PgSink backed by pool. pool may be nil; the sink will
// silently drop records until a non-nil pool is provided.
func NewPgSink(pool *pgxpool.Pool) *PgSink {
	return &PgSink{pool: pool}
}

// SetPool replaces the pool. Call this when the store becomes ready.
func (p *PgSink) SetPool(pool *pgxpool.Pool) {
	p.pool = pool
}

const pgInsertAuditSQL = `
INSERT INTO audit_log (
    at, request_id, actor_kind, actor_user_id, actor_token_id,
    actor_ip, actor_via, actor_peer_uid,
    instance_name, action, target, params, outcome, error, duration_ms, spool_id
) VALUES (
    $1, $2, $3::actor_t, $4, $5,
    $6, $7, $8,
    $9, $10, $11, $12, $13::outcome_t, $14, $15, $16
)
ON CONFLICT (spool_id) DO NOTHING
RETURNING id`

// Write inserts r into audit_log. If the pool is nil or unavailable, the
// error is logged at debug level and nil is returned so the spool sink can
// capture the record instead.
//
// If the spool_id already exists in the table, ErrDuplicate is returned.
func (p *PgSink) Write(ctx context.Context, r Record) error {
	if p.pool == nil {
		slog.Debug("audit: pg sink has no pool, skipping direct insert",
			"action", r.Action, "spool_id", r.SpoolID)
		return nil
	}

	paramsJSON, err := json.Marshal(r.Params)
	if err != nil {
		paramsJSON = []byte("{}")
	}

	var userID, tokenID, reqID, ipStr, errText *string
	if r.UserID != "" {
		s := r.UserID
		userID = &s
	}
	if r.TokenID != "" {
		s := r.TokenID
		tokenID = &s
	}
	if r.RequestID != "" {
		s := r.RequestID
		reqID = &s
	}
	if r.IP != "" {
		s := r.IP
		ipStr = &s
	}
	if r.Error != "" {
		s := r.Error
		errText = &s
	}
	var durMS *int64
	if r.DurationMS != 0 {
		v := r.DurationMS
		durMS = &v
	}
	var spoolID *string
	if r.SpoolID != "" {
		s := r.SpoolID
		spoolID = &s
	}

	var id int64
	rowErr := p.pool.QueryRow(ctx, pgInsertAuditSQL,
		r.At,                 // $1
		reqID,                // $2
		pgActorKind(r.Kind),  // $3
		userID,               // $4
		tokenID,              // $5
		ipStr,                // $6
		string(r.Via),        // $7
		r.PeerUID,            // $8
		r.InstanceName,       // $9
		string(r.Action),     // $10
		r.Target,             // $11
		paramsJSON,           // $12
		pgOutcome(r.Outcome), // $13
		errText,              // $14
		durMS,                // $15
		spoolID,              // $16
	).Scan(&id)

	if errors.Is(rowErr, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING — spool_id already present.
		return ErrDuplicate
	}
	if rowErr != nil {
		// Treat connection errors as transient: log and return nil so the
		// spool sink can capture the record.
		slog.Debug("audit: pg sink insert failed, falling back to spool",
			"err", rowErr, "action", r.Action, "spool_id", r.SpoolID)
		return nil
	}
	_ = id
	return nil
}

// pgActorKind maps Go ActorKind to the Postgres actor_t enum.
func pgActorKind(k ActorKind) string {
	switch k {
	case ActorUser:
		return "user"
	case ActorToken:
		return "api_token"
	case ActorTimer:
		return "timer"
	default: // ActorSystem, ActorRootCLI
		return "system"
	}
}

// pgOutcome maps Go Outcome to the Postgres outcome_t enum.
func pgOutcome(o Outcome) string {
	switch o {
	case OK:
		return "ok"
	case Failed:
		return "failed"
	default: // "started" and any future values
		return "ok"
	}
}

// InsertSpoolRow is a lower-level helper used by ReplaySpool: it inserts one
// record and returns ErrDuplicate when the spool_id already exists.
//
// Unlike PgSink.Write, this returns the database error directly rather than
// falling back, because replay must know whether the insert succeeded.
func InsertSpoolRow(ctx context.Context, pool *pgxpool.Pool, r Record) error {
	paramsJSON, err := json.Marshal(r.Params)
	if err != nil {
		paramsJSON = []byte("{}")
	}

	var userID, tokenID, reqID, ipStr, errText *string
	if r.UserID != "" {
		s := r.UserID
		userID = &s
	}
	if r.TokenID != "" {
		s := r.TokenID
		tokenID = &s
	}
	if r.RequestID != "" {
		s := r.RequestID
		reqID = &s
	}
	if r.IP != "" {
		s := r.IP
		ipStr = &s
	}
	if r.Error != "" {
		s := r.Error
		errText = &s
	}
	var durMS *int64
	if r.DurationMS != 0 {
		v := r.DurationMS
		durMS = &v
	}
	var spoolID *string
	if r.SpoolID != "" {
		s := r.SpoolID
		spoolID = &s
	}

	var id int64
	rowErr := pool.QueryRow(ctx, pgInsertAuditSQL,
		r.At, reqID, pgActorKind(r.Kind), userID, tokenID,
		ipStr, string(r.Via), r.PeerUID,
		r.InstanceName, string(r.Action), r.Target, paramsJSON,
		pgOutcome(r.Outcome), errText, durMS, spoolID,
	).Scan(&id)

	if errors.Is(rowErr, pgx.ErrNoRows) {
		return ErrDuplicate
	}
	if rowErr != nil {
		return fmt.Errorf("audit: insert spool row: %w", rowErr)
	}
	return nil
}
