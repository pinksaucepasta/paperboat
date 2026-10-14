-- +goose Up
ALTER TABLE paperboat.environment_vault_layers
 ADD COLUMN observation_seq bigint NOT NULL DEFAULT 0 CHECK(observation_seq BETWEEN 0 AND 9007199254740991),
 ADD COLUMN observation_digest bytea CHECK(observation_digest IS NULL OR octet_length(observation_digest)=32),
 ADD COLUMN observation bytea CHECK(observation IS NULL OR octet_length(observation) BETWEEN 1 AND 4096),
 ADD CONSTRAINT environment_vault_layer_observation_complete CHECK((observation_seq=0 AND observation_digest IS NULL AND observation IS NULL) OR (observation_seq>0 AND observation_digest IS NOT NULL AND observation IS NOT NULL));
-- +goose Down
ALTER TABLE paperboat.environment_vault_layers DROP CONSTRAINT environment_vault_layer_observation_complete, DROP COLUMN observation, DROP COLUMN observation_digest, DROP COLUMN observation_seq;
