-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.user_machine_access_sessions ADD COLUMN operation_key text;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.user_machine_access_sessions DROP COLUMN operation_key;
