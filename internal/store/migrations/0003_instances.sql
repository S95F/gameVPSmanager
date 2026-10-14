-- +goose Up
CREATE TABLE instances (
    id             uuid PRIMARY KEY,
    name           text NOT NULL UNIQUE,
    game           text NOT NULL,
    yaml_path      text NOT NULL,
    yaml_sha256    bytea,
    manifest_sha256 bytea,
    phase          text NOT NULL DEFAULT 'defined',
    absent_since   timestamptz,
    deleted_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE instance_grants (
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    instance_id uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    role        role_t NOT NULL DEFAULT 'viewer',
    granted_at  timestamptz NOT NULL DEFAULT now(),
    granted_by  uuid REFERENCES users(id),
    PRIMARY KEY (user_id, instance_id)
);
CREATE INDEX instance_grants_instance_idx ON instance_grants (instance_id);

CREATE TABLE port_allocations (
    instance_id  uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    proto        text NOT NULL CHECK (proto IN ('tcp','udp')),
    port         integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    label        text NOT NULL DEFAULT '',
    PRIMARY KEY (instance_id, proto, port)
);

CREATE VIEW port_conflicts AS
SELECT proto, port, count(*) AS n,
       array_agg(instance_id ORDER BY instance_id) AS instance_ids
FROM port_allocations
GROUP BY proto, port
HAVING count(*) > 1;

CREATE VIEW instance_status AS
SELECT i.id, i.name, i.game, i.phase, i.absent_since,
       i.manifest_sha256, i.yaml_sha256
FROM instances i
WHERE i.deleted_at IS NULL;

CREATE TABLE instance_config_keys (
    instance_id  uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    key          text NOT NULL,
    value_sha256 bytea,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, key)
);

CREATE TABLE instance_phase_events (
    id          bigserial PRIMARY KEY,
    instance_id uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    phase       text NOT NULL,
    at          timestamptz NOT NULL DEFAULT now(),
    actor_kind  actor_t NOT NULL DEFAULT 'system',
    actor_user_id uuid REFERENCES users(id),
    note        text NOT NULL DEFAULT ''
);
CREATE INDEX instance_phase_events_idx ON instance_phase_events (instance_id, at DESC);

-- +goose Down
DROP TABLE IF EXISTS instance_phase_events;
DROP TABLE IF EXISTS instance_config_keys;
DROP VIEW IF EXISTS instance_status;
DROP VIEW IF EXISTS port_conflicts;
DROP TABLE IF EXISTS port_allocations;
DROP TABLE IF EXISTS instance_grants;
DROP TABLE IF EXISTS instances;
