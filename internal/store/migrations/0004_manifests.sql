-- +goose Up
CREATE TABLE game_manifests (
    id           uuid PRIMARY KEY,
    game_id      text NOT NULL,
    revision     integer NOT NULL DEFAULT 1,
    sha256       bytea NOT NULL UNIQUE,
    origin       text NOT NULL,
    body         jsonb NOT NULL,
    published_by uuid REFERENCES users(id),
    published_at timestamptz NOT NULL DEFAULT now(),
    superseded_at timestamptz
);
CREATE INDEX game_manifests_game_idx ON game_manifests (game_id, revision DESC);

CREATE TABLE game_manifest_approvals (
    manifest_id  uuid NOT NULL REFERENCES game_manifests(id) ON DELETE CASCADE,
    user_id      uuid NOT NULL REFERENCES users(id),
    approved_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (manifest_id, user_id)
);

-- Latest approved manifest per game, ranked by revision.
CREATE VIEW manifest_resolution AS
SELECT m.game_id, m.id AS manifest_id, m.revision,
       m.sha256, m.origin,
       ROW_NUMBER() OVER (PARTITION BY m.game_id ORDER BY m.revision DESC) AS rank
FROM game_manifests m
WHERE m.superseded_at IS NULL
  AND EXISTS (SELECT 1 FROM game_manifest_approvals a WHERE a.manifest_id = m.id);

CREATE VIEW instances_manifest_drift AS
SELECT i.id AS instance_id, i.name AS instance_name, i.game,
       i.manifest_sha256 AS current_sha256,
       r.manifest_id, r.revision AS available_revision,
       r.sha256 AS available_sha256, r.origin
FROM instances i
JOIN manifest_resolution r ON r.game_id = i.game AND r.rank = 1
WHERE i.deleted_at IS NULL
  AND i.manifest_sha256 IS DISTINCT FROM r.sha256;

-- +goose Down
DROP VIEW IF EXISTS instances_manifest_drift;
DROP VIEW IF EXISTS manifest_resolution;
DROP TABLE IF EXISTS game_manifest_approvals;
DROP TABLE IF EXISTS game_manifests;
