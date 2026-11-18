-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.recording_keys VALIDATE CONSTRAINT recording_keys_chain;

-- +goose Down
SELECT 1;
