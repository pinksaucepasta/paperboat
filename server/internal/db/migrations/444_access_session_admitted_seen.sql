-- +goose Up
-- When a client last named an expired access session as admitted (an open stream still uses it). Without it the server
-- could not tell an expired session with an open stream from an idle one, so every expired session stayed active for
-- the 7-day retention: a machine that ran ~2,300 short commands a day held ~16k of them (prod 2026-11-19).
-- Nullable, no default: a catalog change only.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.user_machine_access_sessions ADD COLUMN admitted_seen_at timestamptz;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.user_machine_access_sessions DROP COLUMN admitted_seen_at;
