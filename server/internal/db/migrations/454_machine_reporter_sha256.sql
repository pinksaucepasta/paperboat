-- +goose Up
-- The SHA-256 of the pb executable a machine runs, reported by its daemon with the version it already reports
-- (reporter_version). The operator Fleet page marks a build official only when this equals the hash of a published,
-- signed release target for that version, platform and architecture (release_target_hashes).
SET lock_timeout = '5s';

ALTER TABLE paperboat.user_machines ADD COLUMN reporter_sha256 text;

-- +goose Down
ALTER TABLE paperboat.user_machines DROP COLUMN reporter_sha256;
