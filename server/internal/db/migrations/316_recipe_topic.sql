-- +goose Up
-- recipe: the topic a recipe machine waits on to refetch GET /v1/recipe-machine (a new revision, a rotated
-- key, or a vault it reads changed) instead of polling. Added NOT VALID, validated in 317.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.machine_topic_generations DROP CONSTRAINT machine_topic_generations_topic_check;
ALTER TABLE paperboat.machine_topic_generations ADD CONSTRAINT machine_topic_generations_topic_check
  CHECK (topic IN ('machine_access', 'peer_network', 'previews', 'tunnels', 'config', 'inventory', 'machine_services', 'recording_policy', 'recipe')) NOT VALID;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'recipe topic is forward-only'; END $$;
-- +goose StatementEnd
