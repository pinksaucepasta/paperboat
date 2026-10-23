-- +goose Up
-- A machine's service snapshot (its listening ports, often changing on a development machine) moved the
-- inventory topic of every machine in its audience, so every daemon of the account re-listed its machines
-- although the machine list carries no services. It moves its own machine_services topic now.
ALTER TABLE paperboat.machine_topic_generations DROP CONSTRAINT machine_topic_generations_topic_check;
ALTER TABLE paperboat.machine_topic_generations ADD CONSTRAINT machine_topic_generations_topic_check
  CHECK (topic IN ('machine_access', 'peer_network', 'previews', 'tunnels', 'config', 'inventory', 'machine_services'));
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_services() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.machine_audience(NEW.machine_id), 'machine_services');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_services() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.machine_audience(NEW.machine_id), 'inventory');
  RETURN NEW;
END $$;
-- +goose StatementEnd
DELETE FROM paperboat.machine_topic_generations WHERE topic = 'machine_services';
ALTER TABLE paperboat.machine_topic_generations DROP CONSTRAINT machine_topic_generations_topic_check;
ALTER TABLE paperboat.machine_topic_generations ADD CONSTRAINT machine_topic_generations_topic_check
  CHECK (topic IN ('machine_access', 'peer_network', 'previews', 'tunnels', 'config', 'inventory'));
