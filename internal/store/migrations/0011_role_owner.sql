-- +goose NO TRANSACTION
-- +goose Up
-- §15.2: the cumulative role hierarchy is viewer ⊂ operator ⊂ admin ⊂ owner.
-- internal/authz gained `owner` in Phase 47; this adds it to the enum so the
-- store can hold it when the dashboard's user management lands in Phase 48.
--
-- Applied only by `gvm db migrate` (rule 8). Outside a transaction because
-- ALTER TYPE ... ADD VALUE cannot run in one on every Postgres this targets,
-- and IF NOT EXISTS makes a replay after a partial run a no-op.
ALTER TYPE role_t ADD VALUE IF NOT EXISTS 'owner';

-- +goose Down
-- Postgres cannot drop an enum value; rolling back would mean rewriting every
-- role_t column. Down is deliberately a no-op.
SELECT 1;
