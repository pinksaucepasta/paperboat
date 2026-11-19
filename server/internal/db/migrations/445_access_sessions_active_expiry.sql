-- +goose NO TRANSACTION
-- +goose Up
-- The expired-session sweeper (RetireExpiredMachineAccessSessions) takes the oldest active sessions in batches; read in
-- expiry order from this partial index it touches exactly the batch, whatever the statistics say (the plain expires_at
-- index also walks every revoked session that the retention keeps).
SET lock_timeout = '5s';
-- A failed concurrent build leaves an INVALID index that IF NOT EXISTS would keep: drop any leftover, then build.
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_access_sessions_active_expiry;
CREATE INDEX CONCURRENTLY user_machine_access_sessions_active_expiry ON paperboat.user_machine_access_sessions (expires_at) WHERE state = 'active';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_access_sessions_active_expiry;
