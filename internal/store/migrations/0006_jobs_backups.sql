-- +goose Up
CREATE TABLE scheduled_jobs (
    id            uuid PRIMARY KEY,
    instance_id   uuid REFERENCES instances(id) ON DELETE CASCADE,
    name          text NOT NULL,
    kind          job_kind_t NOT NULL,
    unit          text NOT NULL UNIQUE,
    on_calendar   text NOT NULL,
    randomized_delay_sec integer NOT NULL DEFAULT 0,
    persistent    boolean NOT NULL DEFAULT false,
    enabled       boolean NOT NULL DEFAULT true,
    applied_on_calendar text,
    applied_at    timestamptz,
    next_elapse   timestamptz,
    last_run_id   bigint
);

CREATE TABLE job_runs (
    id               bigserial PRIMARY KEY,
    scheduled_job_id uuid REFERENCES scheduled_jobs(id) ON DELETE SET NULL,
    instance_id      uuid REFERENCES instances(id) ON DELETE SET NULL,
    instance_name    text NOT NULL DEFAULT '',
    job_name         text,
    kind             job_kind_t  NOT NULL,
    state            job_state_t NOT NULL DEFAULT 'running',
    trigger          job_trigger_t NOT NULL,
    unit             text,
    request_id       text,
    actor_kind       actor_t NOT NULL,
    actor_user_id    uuid REFERENCES users(id),
    actor_token_id   uuid REFERENCES api_tokens(id),
    params           jsonb NOT NULL DEFAULT '{}'::jsonb,
    summary          jsonb NOT NULL DEFAULT '{}'::jsonb,
    run_as           text NOT NULL DEFAULT 'root',
    transient_unit   text,
    log_path         text,
    boot_id          text,
    pid              integer,
    started_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz,
    duration_ms      integer,
    exit_code        integer,
    error            text,
    left_stopped     boolean NOT NULL DEFAULT false,
    spooled          boolean NOT NULL DEFAULT false
);
CREATE INDEX job_runs_instance_idx     ON job_runs (instance_id, started_at DESC);
CREATE INDEX job_runs_active_idx       ON job_runs (state) WHERE state IN ('queued','running');
CREATE INDEX job_runs_attention_idx    ON job_runs (started_at DESC)
    WHERE state IN ('failed','degraded');
CREATE INDEX job_runs_left_stopped_idx ON job_runs (started_at DESC) WHERE left_stopped;

ALTER TABLE scheduled_jobs
    ADD CONSTRAINT scheduled_jobs_last_run_fk
    FOREIGN KEY (last_run_id) REFERENCES job_runs(id) ON DELETE SET NULL;

ALTER TABLE content_snapshots
    ADD CONSTRAINT content_snapshots_job_fk
    FOREIGN KEY (job_id) REFERENCES job_runs(id) ON DELETE SET NULL;

CREATE TABLE host_locks (
    key           text PRIMARY KEY,
    holder_run_id bigint REFERENCES job_runs(id) ON DELETE SET NULL,
    holder_pid    integer,
    acquired_at   timestamptz,
    note          text
);

CREATE TABLE backups (
    id              bigserial PRIMARY KEY,
    instance_id     uuid REFERENCES instances(id) ON DELETE SET NULL,
    instance_name   text NOT NULL,
    job_id          bigint REFERENCES job_runs(id) ON DELETE SET NULL,
    path            text NOT NULL UNIQUE,
    sha256          bytea NOT NULL,
    size_bytes      bigint NOT NULL,
    member_count    integer NOT NULL,
    retention_class retention_t NOT NULL DEFAULT 'routine',
    reason          text NOT NULL DEFAULT '',
    game_manifest_sha256 bytea,
    world_fingerprint    jsonb,
    snapshot_strategy    text,
    freeze_ms       integer,
    world_included  boolean NOT NULL DEFAULT false,
    verified_at     timestamptz,
    verify_result   text,
    remote_url      text,
    remote_at       timestamptz,
    created_by      uuid REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    missing_since   timestamptz
);
CREATE INDEX backups_instance_idx ON backups (instance_name, created_at DESC);
CREATE INDEX backups_preserve_idx ON backups (instance_name)
    WHERE retention_class = 'preserve';

-- +goose Down
DROP TABLE IF EXISTS backups;
DROP TABLE IF EXISTS host_locks;
ALTER TABLE content_snapshots DROP CONSTRAINT IF EXISTS content_snapshots_job_fk;
ALTER TABLE scheduled_jobs DROP CONSTRAINT IF EXISTS scheduled_jobs_last_run_fk;
DROP TABLE IF EXISTS job_runs;
DROP TABLE IF EXISTS scheduled_jobs;
