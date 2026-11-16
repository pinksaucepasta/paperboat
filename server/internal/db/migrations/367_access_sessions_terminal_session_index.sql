-- +goose NO TRANSACTION
-- +goose Up
-- The terminal session peer_policy trigger (366) looks up active access sessions by terminal_session_id once per
-- deleted session row. Without an index each lookup scanned the whole table (22 ms at 41k rows): prod 2026-11-16, the
-- final delete of a removed machine with 1,108 terminal sessions ran past 25 s, held the shared 443 names lock and
-- failed serialization every 18 s, so every preview create and readiness waited behind it (create p50 3.1 s, p95 8.9 s).
SET lock_timeout = '5s';
-- A failed concurrent build leaves an INVALID index that IF NOT EXISTS would keep: drop any leftover, then build.
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_access_sessions_terminal_active;
CREATE INDEX CONCURRENTLY user_machine_access_sessions_terminal_active
 ON paperboat.user_machine_access_sessions (terminal_session_id) WHERE state = 'active';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_access_sessions_terminal_active;
