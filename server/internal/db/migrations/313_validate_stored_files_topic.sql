-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE paperboat.machine_topic_generations VALIDATE CONSTRAINT machine_topic_generations_topic_check;
-- +goose Down
SELECT 1;
