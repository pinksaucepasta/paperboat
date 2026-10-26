-- +goose Up
-- Which client version each machine runs: its runtime observation (every heartbeat) already carries
-- reporter_version, and nothing stored it. A release that changes what clients must decode waits until no machine
-- on an older official version is active (coordinator, 2026-10-26); this column is the count's source. It is
-- written by the heartbeat's existing UPDATE and watched by no topic trigger.
ALTER TABLE paperboat.user_machines ADD COLUMN reporter_version text;

-- +goose Down
ALTER TABLE paperboat.user_machines DROP COLUMN reporter_version;
