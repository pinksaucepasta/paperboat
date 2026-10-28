-- +goose Up
-- Validation scans the child tables without blocking their writers (SHARE UPDATE EXCLUSIVE).
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.device_grants VALIDATE CONSTRAINT device_grants_cli_client_session_id_fkey;
ALTER TABLE paperboat.device_token_redemptions VALIDATE CONSTRAINT device_token_redemptions_cli_client_session_id_fkey;
ALTER TABLE paperboat.lazy_activations VALIDATE CONSTRAINT lazy_activations_policy_id_fkey;
ALTER TABLE paperboat.lazy_activations VALIDATE CONSTRAINT lazy_activations_preview_id_fkey;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'machine deletion FKs are forward-only'; END $$;
-- +goose StatementEnd
