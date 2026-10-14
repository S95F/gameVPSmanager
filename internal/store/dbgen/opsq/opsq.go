// Package opsq contains hand-authored query functions for the gvm_ops role.
// These simulate what sqlc would generate: each function takes a *pgxpool.Pool
// and returns typed results.
//
// gvm_ops has full DML on all tables except audit_log (INSERT+SELECT only) and
// the append-only tables (content_snapshots, instance_phase_events,
// firewall_state).
package opsq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/S95F/gameVPSmanager/internal/audit"
)

// EffectiveRole returns the highest role a user holds for a given instance:
// the instance-level grant if one exists, otherwise the user's global role.
//
// instanceName may be empty, in which case only the global role is returned.
func EffectiveRole(ctx context.Context, pool *pgxpool.Pool, userID, instanceName string) (string, error) {
	if instanceName == "" {
		var role string
		err := pool.QueryRow(ctx,
			`SELECT global_role FROM users WHERE id = $1`, userID).Scan(&role)
		if err != nil {
			return "", fmt.Errorf("opsq: effective role (global): %w", err)
		}
		return role, nil
	}
	var role string
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(
		    (SELECT ig.role::text
		       FROM instance_grants ig
		       JOIN instances i ON i.id = ig.instance_id
		      WHERE ig.user_id = $1 AND i.name = $2),
		    (SELECT u.global_role::text FROM users u WHERE u.id = $1)
		)`, userID, instanceName).Scan(&role)
	if err != nil {
		return "", fmt.Errorf("opsq: effective role: %w", err)
	}
	return role, nil
}

// ReapStaleRuns marks any job_run that was queued or running but whose
// boot_id differs from bootID as failed.
func ReapStaleRuns(ctx context.Context, pool *pgxpool.Pool, bootID string) (int64, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE job_runs
		   SET state = 'failed', finished_at = now(),
		       error = 'daemon or host restarted while this run was in flight'
		 WHERE state IN ('queued','running')
		   AND boot_id IS DISTINCT FROM $1::text`, bootID)
	if err != nil {
		return 0, fmt.Errorf("opsq: reap stale runs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MarkInstancesAbsent sets absent_since on instances not in present.
func MarkInstancesAbsent(ctx context.Context, pool *pgxpool.Pool, present []string) (int64, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE instances SET absent_since = now()
		 WHERE deleted_at IS NULL AND absent_since IS NULL
		   AND NOT (name = ANY($1::text[]))`, present)
	if err != nil {
		return 0, fmt.Errorf("opsq: mark instances absent: %w", err)
	}
	return tag.RowsAffected(), nil
}

// mapActorKind maps Go actor kinds to the Postgres actor_t enum values.
func mapActorKind(k audit.ActorKind) string {
	switch k {
	case audit.ActorUser:
		return "user"
	case audit.ActorToken:
		return "api_token"
	case audit.ActorTimer:
		return "timer"
	default: // ActorSystem, ActorRootCLI, and any future kinds
		return "system"
	}
}

// mapOutcome maps Go outcome values to Postgres outcome_t enum values.
func mapOutcome(o audit.Outcome) string {
	switch o {
	case audit.OK:
		return "ok"
	case audit.Failed:
		return "failed"
	default: // "started" and any other values map to ok
		return "ok"
	}
}

const insertAuditRowSQL = `
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

// InsertAuditRow inserts one audit record into audit_log, deduplicating on
// spool_id. It returns (id, inserted=true) when the row was new, or
// (0, inserted=false) when the spool_id already existed (ON CONFLICT DO NOTHING).
func InsertAuditRow(ctx context.Context, pool *pgxpool.Pool, r audit.Record) (id int64, inserted bool, err error) {
	paramsJSON, perr := json.Marshal(r.Params)
	if perr != nil {
		paramsJSON = []byte("{}")
	}

	var userID, tokenID, reqID *string
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

	viaStr := string(r.Via)

	var errText *string
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

	var ipStr *string
	if r.IP != "" {
		s := r.IP
		ipStr = &s
	}

	rowErr := pool.QueryRow(ctx, insertAuditRowSQL,
		r.At,                  // $1
		reqID,                 // $2
		mapActorKind(r.Kind),  // $3
		userID,                // $4
		tokenID,               // $5
		ipStr,                 // $6
		viaStr,                // $7
		r.PeerUID,             // $8
		r.InstanceName,        // $9
		string(r.Action),      // $10
		r.Target,              // $11
		paramsJSON,            // $12
		mapOutcome(r.Outcome), // $13
		errText,               // $14
		durMS,                 // $15
		spoolID,               // $16
	).Scan(&id)
	if errors.Is(rowErr, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING: duplicate spool_id.
		return 0, false, nil
	}
	if rowErr != nil {
		return 0, false, fmt.Errorf("opsq: insert audit row: %w", rowErr)
	}
	return id, true, nil
}
