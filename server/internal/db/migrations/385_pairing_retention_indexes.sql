-- +goose NO TRANSACTION
-- +goose Up
-- The retention worker deletes abandoned failed pairings and finished installs of replaced or removed machines, 500 per
-- statement; these partial indexes keep each batch off a full scan.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_pairings_failed;
CREATE INDEX CONCURRENTLY user_machine_pairings_failed ON paperboat.user_machine_pairings (updated_at)
  WHERE installation_state = 'failed_retryable';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_pairings_installed;
CREATE INDEX CONCURRENTLY user_machine_pairings_installed ON paperboat.user_machine_pairings (expires_at)
  WHERE installation_state IN ('ready','disconnected','auth_consumed');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_pairings_failed;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_pairings_installed;
