-- +goose Up
-- Retention deletes the oldest configuration generations of a live tunnel (the newest two stay); the self-reference
-- to the previous generation (RESTRICT) refused every such delete. The chain stays enforced where it is used:
-- previous_generation = generation - 1 by CHECK, and a new generation requires generation - 1 to exist
-- (tunnelv1.createResourceConfigGeneration, ErrConfigGenerationChain).
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.tunnel_config_generations DROP CONSTRAINT tunnel_config_generations_tunnel_id_previous_generation_fkey;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.tunnel_config_generations ADD CONSTRAINT tunnel_config_generations_tunnel_id_previous_generation_fkey
 FOREIGN KEY (tunnel_id, previous_generation) REFERENCES paperboat.tunnel_config_generations(tunnel_id, generation) ON DELETE RESTRICT NOT VALID;
