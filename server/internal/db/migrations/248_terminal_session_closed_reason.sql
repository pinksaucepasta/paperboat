-- +goose Up
-- Why a terminal session ended (host_gone, host_closed, exited, user_closed, unreachable_timeout), so the console can
-- say it. Expand only: nullable, no default; the running server never reads or writes it.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.user_machine_terminal_sessions ADD COLUMN closed_reason text;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.user_machine_terminal_sessions DROP COLUMN closed_reason;
