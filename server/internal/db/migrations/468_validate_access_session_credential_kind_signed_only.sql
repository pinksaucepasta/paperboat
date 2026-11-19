-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.user_machine_access_sessions VALIDATE CONSTRAINT user_machine_access_sessions_credential_kind_check;

-- +goose Down
SELECT 1;
