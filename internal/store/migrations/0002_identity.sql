-- +goose Up
CREATE TABLE users (
    id                 uuid PRIMARY KEY,
    email              text NOT NULL UNIQUE,
    display_name       text NOT NULL DEFAULT '',
    password_hash      text,
    must_change_password boolean NOT NULL DEFAULT false,
    global_role        role_t NOT NULL DEFAULT 'viewer',
    totp_enrolled_at   timestamptz,
    totp_secret_ct     bytea,
    totp_key_id        smallint NOT NULL DEFAULT 1,
    last_totp_step     bigint,
    failed_logins      integer NOT NULL DEFAULT 0,
    locked_until       timestamptz,
    last_login_at      timestamptz,
    disabled_at        timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id            text PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ip            inet,
    user_agent    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at    timestamptz
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expiry_idx ON sessions (expires_at);

CREATE TABLE api_tokens (
    id            uuid PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          text NOT NULL,
    prefix        text NOT NULL,
    token_sha256  bytea NOT NULL UNIQUE,
    role_ceiling  role_t NOT NULL DEFAULT 'viewer',
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz,
    last_used_at  timestamptz,
    last_used_ip  inet,
    revoked_at    timestamptz
);
CREATE INDEX api_tokens_user_idx ON api_tokens (user_id);

CREATE TABLE login_attempts (
    id         bigserial PRIMARY KEY,
    email      text NOT NULL,
    ip         inet,
    at         timestamptz NOT NULL DEFAULT now(),
    success    boolean NOT NULL,
    reason     text
);
CREATE INDEX login_attempts_email_idx ON login_attempts (email, at DESC);
CREATE INDEX login_attempts_ip_idx    ON login_attempts (ip, at DESC);

CREATE TABLE user_recovery_codes (
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_sha256 bytea NOT NULL,
    used_at    timestamptz,
    PRIMARY KEY (user_id, code_sha256)
);

-- +goose Down
DROP TABLE IF EXISTS user_recovery_codes;
DROP TABLE IF EXISTS login_attempts;
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
