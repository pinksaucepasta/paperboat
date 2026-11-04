-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.account_topic_generations VALIDATE CONSTRAINT account_topic_generations_topic_check;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'private_routes topic is forward-only'; END $$;
-- +goose StatementEnd
