-- +goose Up
-- All custom types. Created before any table so every table can reference them.

CREATE TYPE role_t AS ENUM ('viewer','operator','admin');
CREATE TYPE actor_t AS ENUM ('user','api_token','system','timer');
CREATE TYPE outcome_t AS ENUM ('ok','failed','denied','degraded');
CREATE TYPE job_kind_t AS ENUM ('backup','update','rebuild','nightly','custom');
CREATE TYPE job_state_t AS ENUM ('queued','running','ok','failed','degraded','cancelled');
CREATE TYPE job_trigger_t AS ENUM ('timer','manual','api','apply');
CREATE TYPE retention_t AS ENUM ('routine','keep','preserve');
CREATE TYPE content_source_t AS ENUM ('workshop','modrinth','curseforge','local','builtin');
CREATE TYPE resolution_path_t AS ENUM ('exact','compatible','latest','override');
CREATE TYPE secret_source_t AS ENUM ('generated','adopted','manual');

-- gvm_ops: everything under this role's schema/table ownership, plus full DML.
-- Default privileges cover tables created AFTER this statement for gvm_ops.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO gvm_ops;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO gvm_ops;

-- +goose Down
DROP TYPE IF EXISTS secret_source_t;
DROP TYPE IF EXISTS resolution_path_t;
DROP TYPE IF EXISTS content_source_t;
DROP TYPE IF EXISTS retention_t;
DROP TYPE IF EXISTS job_trigger_t;
DROP TYPE IF EXISTS job_state_t;
DROP TYPE IF EXISTS job_kind_t;
DROP TYPE IF EXISTS outcome_t;
DROP TYPE IF EXISTS actor_t;
DROP TYPE IF EXISTS role_t;
