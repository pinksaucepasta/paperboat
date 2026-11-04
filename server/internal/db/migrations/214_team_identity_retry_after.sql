-- +goose Up
-- A team whose WorkOS read fails waits here before the next attempt, so it cannot hold the sync's batch slots.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.team_identity_state ADD COLUMN retry_after timestamptz;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.team_identity_state DROP COLUMN retry_after;
