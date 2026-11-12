-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.user_machine_terminal_sessions VALIDATE CONSTRAINT user_machine_terminal_sessions_name_valid;

-- +goose Down
SELECT 1;
