-- +goose Up
-- The stored_files topic moved on every UPDATE OF status or stored_uploaded, also when a write left them unchanged
-- (topic-trigger flap rule: one writer, no publish without a real change). An update publishes only when status,
-- stored_uploaded or stored_prefix actually changes.
SET lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_stored_files() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'UPDATE' AND OLD.status IS NOT DISTINCT FROM NEW.status AND OLD.stored_uploaded IS NOT DISTINCT FROM NEW.stored_uploaded
    AND OLD.stored_prefix IS NOT DISTINCT FROM NEW.stored_prefix THEN
  RETURN NULL;
 END IF;
 IF NEW.stored_prefix IS NOT NULL AND NEW.stored_uploaded AND NEW.status='approved' THEN
  PERFORM paperboat.publish_machine_topic(coalesce((SELECT array_agg(m.id) FROM paperboat.user_machines m
   WHERE m.user_id=NEW.recipient_account AND m.deleted_at IS NULL AND m.revoked_at IS NULL), '{}'::text[]), 'stored_files');
 END IF;
 RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose Down
SET lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_stored_files() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.stored_prefix IS NOT NULL AND NEW.stored_uploaded AND NEW.status='approved' THEN
  PERFORM paperboat.publish_machine_topic(coalesce((SELECT array_agg(m.id) FROM paperboat.user_machines m
   WHERE m.user_id=NEW.recipient_account AND m.deleted_at IS NULL AND m.revoked_at IS NULL), '{}'::text[]), 'stored_files');
 END IF;
 RETURN NULL;
END $$;
-- +goose StatementEnd
