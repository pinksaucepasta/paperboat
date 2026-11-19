-- +goose Up
-- The operator Fleet page's rollout chart: how many machines ran each pb version on each day. One row per day,
-- platform and version, upserted hourly by the fleet snapshot worker (the last write of a day wins): never a
-- per-machine log. failed and rolled_back count machines whose update to this version ended that way and that still
-- run an older one. retention deletes days past retention.fleet_daily.
SET lock_timeout = '5s';

CREATE TABLE paperboat.fleet_version_daily (
    day date NOT NULL,
    platform text NOT NULL CHECK (length(platform) BETWEEN 1 AND 16),
    version text NOT NULL CHECK (length(version) BETWEEN 1 AND 64),
    machines integer NOT NULL DEFAULT 0 CHECK (machines >= 0),
    online integer NOT NULL DEFAULT 0 CHECK (online >= 0),
    failed integer NOT NULL DEFAULT 0 CHECK (failed >= 0),
    rolled_back integer NOT NULL DEFAULT 0 CHECK (rolled_back >= 0),
    PRIMARY KEY (day, platform, version)
);

-- +goose Down
DROP TABLE paperboat.fleet_version_daily;
