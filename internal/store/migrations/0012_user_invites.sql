-- +goose Up
-- Single-use enrolment invites (Phase 48 item 1): `gvm user invite` mints one
-- and prints its URL; the enrolment page redeems it once. The token itself is
-- never stored — token_sha256 is, like api_tokens.
CREATE TABLE user_invites (
    id            uuid PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_sha256  bytea NOT NULL UNIQUE,
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX user_invites_user_idx ON user_invites (user_id);

-- The web tier only RENDERS the enrolment form (validating the token, showing
-- the email); the enrolment itself is written by gvmd (auth.enroll), which
-- holds the master key the TOTP secret is sealed with. So gvm_web reads, and
-- never writes, an invite — D-27's shape again.
GRANT SELECT ON user_invites TO gvm_web;

-- +goose Down
DROP TABLE IF EXISTS user_invites;
