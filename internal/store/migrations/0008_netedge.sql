-- +goose Up
CREATE TABLE firewall_state (
    id               bigserial PRIMARY KEY,
    observed_at      timestamptz NOT NULL DEFAULT now(),
    backend          text    NOT NULL,
    backend_enabled  boolean NOT NULL,
    default_incoming text    NOT NULL,
    default_outgoing text    NOT NULL,
    ipv6             boolean NOT NULL,
    ssh_ports        integer[] NOT NULL,
    ssh_findings     jsonb   NOT NULL DEFAULT '[]'::jsonb,
    ssh_confident    boolean NOT NULL,
    rules            jsonb   NOT NULL,
    n_owned          integer NOT NULL,
    n_foreign        integer NOT NULL,
    n_shadowed       integer NOT NULL DEFAULT 0,
    parse_trusted    boolean NOT NULL,
    applied          boolean NOT NULL,
    plan_sha256      bytea,
    actor_user_id    uuid REFERENCES users(id)
);
CREATE INDEX firewall_state_observed_idx ON firewall_state (observed_at DESC);

CREATE TABLE edge_config (
    id               bigserial PRIMARY KEY,
    rendered_at      timestamptz NOT NULL DEFAULT now(),
    mode             text NOT NULL CHECK (mode IN
                       ('caddy_managed','caddy_snippet','proxy_external','none')),
    tls_mode         text CHECK (tls_mode IN ('acme_http','acme_dns','internal','external')),
    site_address     text,
    caddyfile        text,
    caddyfile_sha256 bytea,
    caddy_version    text,
    applied          boolean NOT NULL DEFAULT false,
    applied_at       timestamptz,
    reload_result    text CHECK (reload_result IN
                       ('ok','validate_failed','reload_failed','rolled_back')),
    cert_status      text CHECK (cert_status IN
                       ('ok','pending','internal','external','failed')),
    error            text,
    actor_user_id    uuid REFERENCES users(id)
);
CREATE INDEX edge_config_rendered_idx ON edge_config (rendered_at DESC);

-- +goose Down
DROP TABLE IF EXISTS edge_config;
DROP TABLE IF EXISTS firewall_state;
