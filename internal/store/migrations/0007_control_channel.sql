-- +goose Up
CREATE TABLE shutdown_reports (
    id            bigserial PRIMARY KEY,
    instance_id   uuid REFERENCES instances(id) ON DELETE SET NULL,
    instance_name text NOT NULL,
    reason        text NOT NULL,
    actor         text NOT NULL,
    started_at    timestamptz NOT NULL,
    finished_at   timestamptz NOT NULL,
    duration_ms   integer NOT NULL,
    final_rung    text NOT NULL,
    degraded      boolean NOT NULL,
    exit_code     integer,
    exit_signal   text,
    extended_usec bigint NOT NULL DEFAULT 0,
    steps         jsonb NOT NULL DEFAULT '[]'::jsonb,
    console_tail  text[] NOT NULL DEFAULT '{}',
    source_offset bigint NOT NULL,
    UNIQUE (instance_name, started_at)
);
CREATE INDEX shutdown_reports_instance_idx ON shutdown_reports (instance_id, finished_at DESC);
CREATE INDEX shutdown_reports_degraded_idx ON shutdown_reports (finished_at DESC) WHERE degraded;

CREATE TABLE log_ingest_offsets (
    instance_name text PRIMARY KEY,
    path          text   NOT NULL,
    offset_bytes  bigint NOT NULL DEFAULT 0,
    inode         bigint,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS log_ingest_offsets;
DROP TABLE IF EXISTS shutdown_reports;
