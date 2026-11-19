-- +goose NO TRANSACTION
-- +goose Up
-- RetireAccessSessionsOfEndedMachines takes one ended machine's expired active sessions in expiry order, a batch at a
-- time: this partial index answers it with one range scan per machine, whatever the statistics say (right after a
-- burst of 20k sessions, before autoanalyze, a statistics-driven plan ran into the 30 s statement timeout).
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_access_sessions_machine_active_expiry;
CREATE INDEX CONCURRENTLY user_machine_access_sessions_machine_active_expiry ON paperboat.user_machine_access_sessions (user_machine_id, expires_at) WHERE state = 'active';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_access_sessions_machine_active_expiry;
