-- +goose Up
REVOKE ALL ON audit_log FROM gvm_ops, gvm_web;
GRANT SELECT, INSERT ON audit_log TO gvm_ops, gvm_web;
GRANT USAGE, SELECT ON SEQUENCE audit_log_id_seq TO gvm_ops, gvm_web;

REVOKE UPDATE, DELETE ON content_snapshots      FROM gvm_ops, gvm_web;
REVOKE UPDATE, DELETE ON instance_phase_events  FROM gvm_ops, gvm_web;
REVOKE UPDATE, DELETE ON firewall_state         FROM gvm_ops, gvm_web;

GRANT SELECT (id, email, display_name, password_hash, must_change_password,
              global_role, totp_enrolled_at, failed_logins, locked_until,
              last_login_at, disabled_at, created_at)
    ON users TO gvm_web;
GRANT UPDATE (failed_logins, locked_until, last_login_at) ON users TO gvm_web;

GRANT SELECT, INSERT, UPDATE, DELETE ON sessions TO gvm_web;

GRANT SELECT (id, user_id, name, prefix, token_sha256, role_ceiling,
              created_at, expires_at, last_used_at, revoked_at)
    ON api_tokens TO gvm_web;
GRANT UPDATE (last_used_at, last_used_ip) ON api_tokens TO gvm_web;

GRANT SELECT, INSERT ON login_attempts TO gvm_web;
GRANT USAGE, SELECT ON SEQUENCE login_attempts_id_seq TO gvm_web;

GRANT SELECT, INSERT, UPDATE (consumed_at) ON action_plans TO gvm_web;

GRANT SELECT ON instances, instance_grants, instance_status, instance_config_keys,
                instance_phase_events, port_allocations, port_conflicts,
                game_manifests, game_manifest_approvals, manifest_resolution,
                instances_manifest_drift,
                content_lists, content_items, content_snapshots, content_blocked,
                scheduled_jobs, job_runs, backups, shutdown_reports,
                firewall_state, edge_config,
                secret_refs, secrets_needing_rotation, authz_permissions
    TO gvm_web;

-- +goose Down
-- Grants are not easily reverted; this is a no-op down migration.
