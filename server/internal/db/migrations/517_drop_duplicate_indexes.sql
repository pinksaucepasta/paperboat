-- +goose NO TRANSACTION
-- +goose Up
-- Each has a twin on the same columns and predicate that stays: the first beside a unique index, the next two beside
-- the _expires indexes of 159, the fourth beside the primary key, the last beside a unique constraint (a btree is read
-- in both directions).
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.account_e2ee_keys_cli_session_idx;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_config_credentials_expiry;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_config_repository_access_operations_expiry;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.idx_github_oauth_tokens_user_id;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.release_authority_bundles_latest;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.account_e2ee_keys_cli_session_idx;
CREATE INDEX CONCURRENTLY account_e2ee_keys_cli_session_idx ON paperboat.account_e2ee_keys (cli_client_session_id) WHERE cli_client_session_id IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_config_credentials_expiry;
CREATE INDEX CONCURRENTLY control_config_credentials_expiry ON paperboat.control_config_credentials (expires_at);
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_config_repository_access_operations_expiry;
CREATE INDEX CONCURRENTLY control_config_repository_access_operations_expiry ON paperboat.control_config_repository_access_operations (expires_at);
DROP INDEX CONCURRENTLY IF EXISTS paperboat.idx_github_oauth_tokens_user_id;
CREATE INDEX CONCURRENTLY idx_github_oauth_tokens_user_id ON paperboat.github_oauth_tokens (user_id);
DROP INDEX CONCURRENTLY IF EXISTS paperboat.release_authority_bundles_latest;
CREATE INDEX CONCURRENTLY release_authority_bundles_latest ON paperboat.release_authority_bundles (release_id, platform, architecture, policy_revision DESC);
