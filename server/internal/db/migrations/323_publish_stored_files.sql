-- +goose Up
SET lock_timeout = '5s';
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_stored_files() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.stored_prefix IS NOT NULL AND NEW.stored_uploaded AND NEW.status='approved' THEN
  PERFORM paperboat.publish_machine_topic(coalesce((SELECT array_agg(m.id) FROM paperboat.user_machines m
   WHERE m.user_id=NEW.recipient_account AND m.deleted_at IS NULL AND m.revoked_at IS NULL), '{}'::text[]), 'stored_files');
 END IF;
 RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER team_inbox_stored_files AFTER INSERT OR UPDATE OF status,stored_uploaded ON paperboat.team_inbox_requests
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_stored_files();
-- +goose Down
DROP TRIGGER team_inbox_stored_files ON paperboat.team_inbox_requests;
DROP FUNCTION paperboat.publish_stored_files();
