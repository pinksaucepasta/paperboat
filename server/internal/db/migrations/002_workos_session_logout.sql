-- +goose Up
ALTER TABLE paperboat.sessions
  ADD COLUMN workos_session_id text,
  ADD COLUMN workos_refresh_token_ciphertext bytea,
  ADD COLUMN workos_access_expires_at timestamptz;

ALTER TABLE paperboat.sessions
  ADD CONSTRAINT sessions_workos_session_id_format
  CHECK (workos_session_id IS NULL OR workos_session_id ~ '^session_[A-Za-z0-9]{1,128}$');

-- +goose Down
ALTER TABLE paperboat.sessions DROP CONSTRAINT sessions_workos_session_id_format;
ALTER TABLE paperboat.sessions DROP COLUMN workos_session_id;
ALTER TABLE paperboat.sessions DROP COLUMN workos_refresh_token_ciphertext;
ALTER TABLE paperboat.sessions DROP COLUMN workos_access_expires_at;
