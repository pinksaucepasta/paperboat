-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.machine_topic_generations VALIDATE CONSTRAINT machine_topic_generations_topic_check;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'recording_policy topic is forward-only'; END $$;
-- +goose StatementEnd
