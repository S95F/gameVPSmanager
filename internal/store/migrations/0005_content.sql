-- +goose Up
CREATE TABLE content_lists (
    instance_id uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    name        text NOT NULL,
    source      content_source_t NOT NULL,
    collection  text,
    verbatim_activation boolean NOT NULL DEFAULT false,
    lock_sha256 bytea,
    resolved_at timestamptz,
    PRIMARY KEY (instance_id, name)
);

CREATE TABLE content_items (
    instance_id    uuid NOT NULL,
    list_name      text NOT NULL,
    acquire_id     text NOT NULL,
    ordinal        integer NOT NULL,
    pin            text,
    note           text NOT NULL DEFAULT '',
    enabled        boolean NOT NULL DEFAULT true,
    requires       text[] NOT NULL DEFAULT '{}',
    activation_ids text[] NOT NULL DEFAULT '{}',
    dropped_ids    text[] NOT NULL DEFAULT '{}',
    status         text NOT NULL DEFAULT 'unresolved'
                   CHECK (status IN ('unresolved','ok','multi','check','missing',
                                     'empty','blocked')),
    ambiguous      boolean NOT NULL DEFAULT false,
    evidence       text[] NOT NULL DEFAULT '{}',
    artifact_kind  text,
    path           text,
    digest_algo    text,
    digest_hex     text,
    size_bytes     bigint,
    upstream_updated_at timestamptz,
    upstream_order integer,
    resolution     resolution_path_t,
    blocked        jsonb,
    implied_by     text,
    resolved_at    timestamptz,
    PRIMARY KEY (instance_id, list_name, acquire_id),
    FOREIGN KEY (instance_id, list_name)
        REFERENCES content_lists(instance_id, name) ON DELETE CASCADE
);
CREATE UNIQUE INDEX content_items_ordinal_idx
    ON content_items (instance_id, list_name, ordinal);
CREATE INDEX content_items_attention_idx ON content_items (instance_id)
    WHERE status IN ('check','missing','empty','blocked');

CREATE TABLE content_snapshots (
    id             bigserial PRIMARY KEY,
    instance_id    uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    list_name      text NOT NULL,
    job_id         bigint,
    taken_at       timestamptz NOT NULL DEFAULT now(),
    reason         text NOT NULL,
    impact         text NOT NULL,
    request_id     text,
    acquire_ids    text[] NOT NULL,
    activation_ids text[] NOT NULL,
    lock_sha256    bytea NOT NULL
);
CREATE INDEX content_snapshots_idx
    ON content_snapshots (instance_id, list_name, taken_at DESC);

CREATE VIEW content_blocked AS
SELECT instance_id, list_name, acquire_id,
       blocked->>'reason'      AS reason,
       blocked->>'file_name'   AS file_name,
       blocked->>'browser_url' AS browser_url
FROM content_items WHERE blocked IS NOT NULL;

-- +goose Down
DROP VIEW IF EXISTS content_blocked;
DROP TABLE IF EXISTS content_snapshots;
DROP TABLE IF EXISTS content_items;
DROP TABLE IF EXISTS content_lists;
