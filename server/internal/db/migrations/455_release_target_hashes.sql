-- +goose Up
-- The hash of every signed pb release target the server has published, one row per version, platform, architecture
-- and hash. The origin's TUF targets.json lists only the newest target of each platform, so the fleet snapshot worker
-- records what it reads (targets.hashes.sha256, which the client's updater verifies before it installs) and the
-- Fleet page compares machines against all of it. Bounded by releases: a handful of rows each.
SET lock_timeout = '5s';

CREATE TABLE paperboat.release_target_hashes (
    version text NOT NULL CHECK (length(version) BETWEEN 1 AND 64),
    platform text NOT NULL CHECK (platform IN ('darwin', 'linux', 'windows')),
    architecture text NOT NULL CHECK (architecture IN ('amd64', 'arm64')),
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (version, platform, architecture, sha256)
);

-- +goose Down
DROP TABLE paperboat.release_target_hashes;
