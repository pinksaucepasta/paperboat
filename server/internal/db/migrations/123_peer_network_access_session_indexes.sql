-- +goose Up
-- The peer-network triggers (machine, CLI session, certificate, environment) look up the account's machine access
-- sessions by CLI session, machine or environment; only partial indexes existed, so the lookups scanned the table and
-- a serializable transaction held a table-wide predicate lock on it. Removing a machine then conflicted with every
-- concurrent access-session write and failed all of its retries.
CREATE INDEX user_machine_access_sessions_account_cli ON paperboat.user_machine_access_sessions(user_id, cli_client_session_id, user_machine_id);
CREATE INDEX user_machine_access_sessions_account_machine ON paperboat.user_machine_access_sessions(user_id, user_machine_id, cli_client_session_id);
CREATE INDEX user_machine_access_sessions_account_environment ON paperboat.user_machine_access_sessions(user_id, environment_id);

-- +goose Down
DROP INDEX paperboat.user_machine_access_sessions_account_environment;
DROP INDEX paperboat.user_machine_access_sessions_account_machine;
DROP INDEX paperboat.user_machine_access_sessions_account_cli;
