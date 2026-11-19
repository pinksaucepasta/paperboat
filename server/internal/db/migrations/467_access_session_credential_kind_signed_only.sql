-- +goose Up
-- The external-issuer credential path is gone (92e38f29a); the only kind left is control_plane_signed.
-- Prod holds no external_issuer row (checked read-only). Added NOT VALID; 468 validates it.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.user_machine_access_sessions DROP CONSTRAINT user_machine_access_sessions_credential_kind_check;
ALTER TABLE paperboat.user_machine_access_sessions ADD CONSTRAINT user_machine_access_sessions_credential_kind_check
  CHECK (credential_kind = 'control_plane_signed') NOT VALID;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION '467 is forward-only: the removed external_issuer kind is not coming back'; END $$;
-- +goose StatementEnd
