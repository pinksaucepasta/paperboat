-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE paperboat.team_buckets ADD COLUMN use_files boolean NOT NULL DEFAULT false;
-- +goose Down
SET lock_timeout = '5s';
ALTER TABLE paperboat.team_buckets DROP COLUMN use_files;
