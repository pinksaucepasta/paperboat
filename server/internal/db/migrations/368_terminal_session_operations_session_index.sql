-- +goose NO TRANSACTION
-- +goose Up
-- Deleting a terminal session cascades to its operations by terminal_session_id; the only index on it is partial
-- (pending), so each cascaded session scanned the whole table (2.3 ms at 9k rows, 1,108 times for one machine, 367).
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_terminal_session_operations_session;
CREATE INDEX CONCURRENTLY user_machine_terminal_session_operations_session
 ON paperboat.user_machine_terminal_session_operations (terminal_session_id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_terminal_session_operations_session;
