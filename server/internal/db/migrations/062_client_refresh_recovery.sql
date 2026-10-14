-- +goose Up
CREATE TABLE paperboat.client_refresh_redemptions (
 token_hash text PRIMARY KEY REFERENCES paperboat.cli_refresh_tokens(token_hash) ON DELETE CASCADE,
 attempt_hash text NOT NULL,
 cli_client_session_id text NOT NULL REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE,
 response_ciphertext bytea NOT NULL,
 access_expires_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL
);
CREATE INDEX client_refresh_redemptions_expiry ON paperboat.client_refresh_redemptions(expires_at);
-- +goose Down
DROP TABLE paperboat.client_refresh_redemptions;
