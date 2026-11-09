-- +goose Up
SET lock_timeout = '5s';
-- Whether commands on the team's machines may list the names (never values) of the team workspace's ENV.
ALTER TABLE paperboat.teams ADD COLUMN env_names_listable boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE paperboat.teams DROP COLUMN env_names_listable;
