-- +goose Up
DROP TABLE paperboat.auth_enrollment_redemptions;
ALTER TABLE paperboat.device_grants ADD COLUMN cli_client_session_id text REFERENCES paperboat.cli_client_sessions(id);
CREATE TABLE paperboat.device_token_redemptions (
 grant_id text PRIMARY KEY REFERENCES paperboat.device_grants(id) ON DELETE CASCADE,
 cli_client_session_id text NOT NULL REFERENCES paperboat.cli_client_sessions(id),
 response_ciphertext bytea NOT NULL,
 access_expires_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL
);
CREATE INDEX device_token_redemptions_expiry ON paperboat.device_token_redemptions(expires_at);
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 RAISE EXCEPTION 'device authorization cutover is irreversible; roll back the release instead';
END $$;
-- +goose StatementEnd
