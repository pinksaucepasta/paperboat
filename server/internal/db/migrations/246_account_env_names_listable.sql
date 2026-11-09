-- +goose Up
SET lock_timeout = '5s';
-- Whether commands on the account's machines may list the names (never values) of its personal-workspace ENV.
ALTER TABLE paperboat.users ADD COLUMN env_names_listable boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE paperboat.users DROP COLUMN env_names_listable;
