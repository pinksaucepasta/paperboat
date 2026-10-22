-- +goose Up
-- The terminal catalog starts from the account's own sessions.
CREATE INDEX user_machine_terminal_sessions_owner ON paperboat.user_machine_terminal_sessions(owner_account) WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX paperboat.user_machine_terminal_sessions_owner;
