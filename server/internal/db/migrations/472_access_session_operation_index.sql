-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_access_sessions_live_operation;
CREATE INDEX CONCURRENTLY user_machine_access_sessions_live_operation
  ON paperboat.user_machine_access_sessions (operation_key, expires_at DESC)
  WHERE state = 'active' AND operation_key IS NOT NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_access_sessions_live_operation;
