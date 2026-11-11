-- +goose Up
-- recording_policy: the topic a machine waits on to refetch its recording policy (shared/recording Policy).
-- Added NOT VALID, validated in 263.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.machine_topic_generations DROP CONSTRAINT machine_topic_generations_topic_check;
ALTER TABLE paperboat.machine_topic_generations ADD CONSTRAINT machine_topic_generations_topic_check
  CHECK (topic IN ('machine_access', 'peer_network', 'previews', 'tunnels', 'config', 'inventory', 'machine_services', 'recording_policy')) NOT VALID;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'recording_policy topic is forward-only'; END $$;
-- +goose StatementEnd
