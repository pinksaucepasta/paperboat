-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE paperboat.machine_topic_generations DROP CONSTRAINT machine_topic_generations_topic_check;
ALTER TABLE paperboat.machine_topic_generations ADD CONSTRAINT machine_topic_generations_topic_check
 CHECK (topic IN ('machine_access','peer_network','previews','tunnels','config','inventory','machine_services','recording_policy','recipe','stored_files')) NOT VALID;
-- +goose Down
SELECT 1;
