-- +goose Up
-- A terminal session name is what a person typed ("Zoë build 1"): any 1-64 characters without control characters
-- (the same rule as shared/sessionname; the application is stricter and also refuses format characters).
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.user_machine_terminal_sessions
  DROP CONSTRAINT user_machine_terminal_sessions_name_check,
  ADD CONSTRAINT user_machine_terminal_sessions_name_valid CHECK (char_length(name) BETWEEN 1 AND 64 AND name !~ '[[:cntrl:]]') NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.user_machine_terminal_sessions
  DROP CONSTRAINT user_machine_terminal_sessions_name_valid,
  ADD CONSTRAINT user_machine_terminal_sessions_name_check CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$') NOT VALID;
