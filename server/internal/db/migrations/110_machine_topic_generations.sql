-- +goose Up
-- A host learns that something it caches changed by waiting on its topic generations instead of
-- polling: every write that changes what a machine reads bumps the generation of that topic for the
-- machines concerned, and the bump wakes any request waiting on them. Counters are persisted, so a
-- reconnecting host compares what it last saw with the current value and misses nothing.
CREATE TABLE paperboat.machine_topic_generations (
    machine_id text NOT NULL REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
    topic text NOT NULL CHECK (topic IN ('machine_access', 'peer_network', 'previews', 'tunnels', 'config', 'inventory')),
    generation bigint NOT NULL CHECK (generation > 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (machine_id, topic)
);

-- publish_machine_topic bumps the topic generation for each machine and notifies listeners once the
-- surrounding transaction commits. It is the single implementation behind the Go Publish helper and
-- the triggers below.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_machine_topic(machine_ids text[], wanted_topic text) RETURNS void
    LANGUAGE sql AS $$
 WITH bumped AS (
  INSERT INTO paperboat.machine_topic_generations(machine_id,topic,generation,updated_at)
  SELECT m.id,wanted_topic,1,now() FROM paperboat.user_machines m
  WHERE m.id = ANY (machine_ids)
  ON CONFLICT (machine_id,topic) DO UPDATE SET generation=paperboat.machine_topic_generations.generation+1,updated_at=now()
  RETURNING machine_id)
 SELECT pg_notify('machine_events',machine_id) FROM bumped;
$$;
-- +goose StatementEnd

-- A machine's access sessions (granted, used, revoked, expired) and who may use it change what its
-- host has to accept.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_machine_access_session() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY[NEW.user_machine_id], 'machine_access');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER user_machine_access_sessions_publish_insert
    AFTER INSERT ON paperboat.user_machine_access_sessions
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_machine_access_session();
CREATE TRIGGER user_machine_access_sessions_publish_update
    AFTER UPDATE OF state ON paperboat.user_machine_access_sessions
    FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION paperboat.publish_machine_access_session();

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_machine_access_grant() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY[NEW.machine_id], 'machine_access');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER team_machine_grants_publish
    AFTER INSERT OR UPDATE ON paperboat.team_machine_grants
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_machine_access_grant();

-- A preview appearing, stopping or changing target is something its owner machine has to act on;
-- lease renewals are not.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_machine_preview() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY[NEW.owner_machine_id], 'previews');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER preview_leases_publish_insert
    AFTER INSERT ON paperboat.preview_leases
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_machine_preview();
CREATE TRIGGER preview_leases_publish_update
    AFTER UPDATE OF terminal_state, access_mode, target_scheme, target_address ON paperboat.preview_leases
    FOR EACH ROW WHEN (OLD.terminal_state IS DISTINCT FROM NEW.terminal_state OR OLD.access_mode IS DISTINCT FROM NEW.access_mode OR OLD.target_scheme IS DISTINCT FROM NEW.target_scheme OR OLD.target_address IS DISTINCT FROM NEW.target_address)
    EXECUTE FUNCTION paperboat.publish_machine_preview();

-- +goose Down
DROP TRIGGER preview_leases_publish_update ON paperboat.preview_leases;
DROP TRIGGER preview_leases_publish_insert ON paperboat.preview_leases;
DROP FUNCTION paperboat.publish_machine_preview();
DROP TRIGGER team_machine_grants_publish ON paperboat.team_machine_grants;
DROP FUNCTION paperboat.publish_machine_access_grant();
DROP TRIGGER user_machine_access_sessions_publish_update ON paperboat.user_machine_access_sessions;
DROP TRIGGER user_machine_access_sessions_publish_insert ON paperboat.user_machine_access_sessions;
DROP FUNCTION paperboat.publish_machine_access_session();
DROP FUNCTION paperboat.publish_machine_topic(text[], text);
DROP TABLE paperboat.machine_topic_generations;
