-- +goose Up
CREATE TABLE secret_refs (
    scope       text NOT NULL CHECK (scope IN ('instance','host')),
    scope_key   text NOT NULL,
    name        text NOT NULL,
    path        text NOT NULL,
    fingerprint bytea,
    key_id      smallint NOT NULL DEFAULT 1,
    source      secret_source_t NOT NULL,
    rotated_at      timestamptz,
    rotation_due_at timestamptz,
    written_by  uuid REFERENCES users(id),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, scope_key, name)
);

CREATE VIEW secrets_needing_rotation AS
SELECT * FROM secret_refs
WHERE (source = 'adopted' AND rotated_at IS NULL)
   OR (rotation_due_at IS NOT NULL AND rotation_due_at < now());

CREATE TABLE audit_log (
    id           bigserial PRIMARY KEY,
    at           timestamptz NOT NULL DEFAULT now(),
    request_id   text,
    parent_id    bigint REFERENCES audit_log(id),
    actor_kind     actor_t NOT NULL,
    actor_user_id  uuid REFERENCES users(id),
    actor_token_id uuid REFERENCES api_tokens(id),
    actor_ip       inet,
    actor_via      text NOT NULL DEFAULT 'cli',
    actor_peer_uid integer,
    instance_id   uuid REFERENCES instances(id) ON DELETE SET NULL,
    instance_name text NOT NULL DEFAULT '',
    action   text NOT NULL,
    target   text NOT NULL DEFAULT '',
    params   jsonb NOT NULL DEFAULT '{}'::jsonb,
    outcome  outcome_t NOT NULL,
    error    text,
    duration_ms integer,
    spool_id text
);
CREATE INDEX audit_log_at_idx       ON audit_log (at DESC);
CREATE INDEX audit_log_instance_idx ON audit_log (instance_id, at DESC);
CREATE INDEX audit_log_actor_idx    ON audit_log (actor_user_id, at DESC);
CREATE INDEX audit_log_action_idx   ON audit_log (action, at DESC);
CREATE INDEX audit_log_request_idx  ON audit_log (request_id) WHERE request_id IS NOT NULL;
CREATE UNIQUE INDEX audit_log_spool_idx ON audit_log (spool_id) WHERE spool_id IS NOT NULL;

CREATE TABLE spool_replays (
    file        text PRIMARY KEY,
    replayed_at timestamptz NOT NULL DEFAULT now(),
    rows_ok     integer NOT NULL,
    rows_dup    integer NOT NULL
);

CREATE TABLE authz_permissions (
    role       role_t NOT NULL,
    permission text   NOT NULL,
    PRIMARY KEY (role, permission)
);

CREATE TABLE action_plans (
    id            text PRIMARY KEY,
    instance_id   uuid REFERENCES instances(id) ON DELETE CASCADE,
    kind          text NOT NULL,
    spec_sha256   bytea,
    payload       jsonb NOT NULL,
    created_by    uuid REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    consumed_at   timestamptz
);
CREATE INDEX action_plans_expiry_idx ON action_plans (expires_at);

-- +goose Down
DROP TABLE IF EXISTS action_plans;
DROP TABLE IF EXISTS authz_permissions;
DROP TABLE IF EXISTS spool_replays;
DROP TABLE IF EXISTS audit_log;
DROP VIEW IF EXISTS secrets_needing_rotation;
DROP TABLE IF EXISTS secret_refs;
