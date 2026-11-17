-- +goose NO TRANSACTION
-- +goose Up
-- The retention worker deletes soft-deleted terminal sessions and detached superseded control routes, 500 per statement; these partial indexes keep each batch off a full scan.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_terminal_sessions_deleted;
CREATE INDEX CONCURRENTLY user_machine_terminal_sessions_deleted ON paperboat.user_machine_terminal_sessions (deleted_at) WHERE deleted_at IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_routes_detached;
CREATE INDEX CONCURRENTLY control_routes_detached ON paperboat.control_routes (updated_at) WHERE desired_state = 'detached';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_terminal_sessions_deleted;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_routes_detached;
