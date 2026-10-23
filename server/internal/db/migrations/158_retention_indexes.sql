-- +goose Up
-- The retention worker (internal/retention) deletes past each table's window in bounded batches; each window and
-- each "still referenced" check is served by an index, so a batch never scans its table.
CREATE INDEX control_config_credentials_expires ON paperboat.control_config_credentials (expires_at);
CREATE INDEX control_config_repository_access_operations_expires ON paperboat.control_config_repository_access_operations (expires_at);
CREATE INDEX managed_ssh_operations_created ON paperboat.managed_ssh_operations (created_at);
CREATE INDEX operations_completed ON paperboat.operations (completed_at) WHERE completed_at IS NOT NULL;
CREATE INDEX user_machine_access_sessions_expires ON paperboat.user_machine_access_sessions (expires_at);
CREATE INDEX sessions_expires ON paperboat.sessions (expires_at);
CREATE INDEX cli_client_sessions_revoked ON paperboat.cli_client_sessions (revoked_at) WHERE revoked_at IS NOT NULL;
CREATE INDEX cli_access_tokens_expires ON paperboat.cli_access_tokens (expires_at);
CREATE INDEX cli_refresh_tokens_expires ON paperboat.cli_refresh_tokens (expires_at);
CREATE INDEX control_usage_receipts_acknowledged ON paperboat.control_usage_receipts (acknowledged_at) WHERE acknowledged_at IS NOT NULL;
CREATE INDEX team_machine_requests_decided ON paperboat.team_machine_requests (decided_at) WHERE decided_at IS NOT NULL;
CREATE INDEX stripe_events_processed ON paperboat.stripe_events (processed_at);
CREATE INDEX device_grants_cli_session ON paperboat.device_grants (cli_client_session_id) WHERE cli_client_session_id IS NOT NULL;
CREATE INDEX device_token_redemptions_cli_session ON paperboat.device_token_redemptions (cli_client_session_id);

-- +goose Down
DROP INDEX paperboat.device_token_redemptions_cli_session;
DROP INDEX paperboat.device_grants_cli_session;
DROP INDEX paperboat.stripe_events_processed;
DROP INDEX paperboat.team_machine_requests_decided;
DROP INDEX paperboat.control_usage_receipts_acknowledged;
DROP INDEX paperboat.cli_refresh_tokens_expires;
DROP INDEX paperboat.cli_access_tokens_expires;
DROP INDEX paperboat.cli_client_sessions_revoked;
DROP INDEX paperboat.sessions_expires;
DROP INDEX paperboat.user_machine_access_sessions_expires;
DROP INDEX paperboat.operations_completed;
DROP INDEX paperboat.managed_ssh_operations_created;
DROP INDEX paperboat.control_config_repository_access_operations_expires;
DROP INDEX paperboat.control_config_credentials_expires;
