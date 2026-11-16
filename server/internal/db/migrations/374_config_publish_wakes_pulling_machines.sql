-- +goose Up
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
-- A machine that published to a config repository did not tell the machines pulling from it: they found the
-- commit on their next 60 s remote poll (FIX-CONFIGLATENCY, 2026-11-17: Mac applied 64-75 s after the write on L).
-- A new published revision now moves the config topic of every other machine pulling from that repository, and
-- their config workers sync on it (Syncthing announces an index update to peers instead of waiting for a rescan).
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_config_repository_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(
    (SELECT coalesce(array_agg(peer.machine_id), '{}'::text[])
       FROM paperboat.control_config_assignments publisher
       JOIN paperboat.control_config_assignments peer
         ON peer.repository_id = coalesce(publisher.push_repository_id, publisher.repository_id)
      WHERE publisher.id = NEW.assignment_id AND peer.machine_id <> NEW.machine_id
        AND peer.revoked_at IS NULL AND peer.mode <> 'push_only'),
    'config');
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- Every status report rewrites the column; only a new revision moves the topic (one writer, no flapping).
CREATE TRIGGER control_config_sync_statuses_publish_revision_insert
AFTER INSERT ON paperboat.control_config_sync_statuses
FOR EACH ROW WHEN (NEW.last_published_revision IS NOT NULL)
EXECUTE FUNCTION paperboat.publish_config_repository_revision();
CREATE TRIGGER control_config_sync_statuses_publish_revision_update
AFTER UPDATE OF last_published_revision ON paperboat.control_config_sync_statuses
FOR EACH ROW WHEN (NEW.last_published_revision IS NOT NULL
  AND OLD.last_published_revision IS DISTINCT FROM NEW.last_published_revision)
EXECUTE FUNCTION paperboat.publish_config_repository_revision();

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
DROP TRIGGER control_config_sync_statuses_publish_revision_update ON paperboat.control_config_sync_statuses;
DROP TRIGGER control_config_sync_statuses_publish_revision_insert ON paperboat.control_config_sync_statuses;
DROP FUNCTION paperboat.publish_config_repository_revision();
