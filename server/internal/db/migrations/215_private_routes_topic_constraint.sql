-- +goose Up
-- private_routes: the topic a machine waits on instead of re-fetching its private-access routes every second.
-- It moves per account (the accessors are every machine of the route's account); added NOT VALID, validated in 216.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.account_topic_generations DROP CONSTRAINT account_topic_generations_topic_check;
ALTER TABLE paperboat.account_topic_generations ADD CONSTRAINT account_topic_generations_topic_check
  CHECK (topic IN ('machine_access', 'peer_network', 'previews', 'tunnels', 'config', 'inventory', 'machine_services', 'private_routes')) NOT VALID;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'private_routes topic is forward-only'; END $$;
-- +goose StatementEnd
