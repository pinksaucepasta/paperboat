-- +goose Up
-- Team setting: only owners and admins may start a recording by hand (default: anyone attached to the session).
SET lock_timeout = '5s';
ALTER TABLE paperboat.teams ADD COLUMN recording_admins_only boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE paperboat.teams DROP COLUMN recording_admins_only;
