-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE paperboat.team_inbox_requests VALIDATE CONSTRAINT team_inbox_stored_metadata_bound;
-- +goose Down
SELECT 1;
