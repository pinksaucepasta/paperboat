-- +goose Up
-- Sharing a machine into a team, or withdrawing it, changes which policy records it: move its recording_policy topic.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_machine_binding_recording_policy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY[NEW.resource_id], 'recording_policy');
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER team_resource_bindings_publish_recording_policy
    AFTER INSERT OR UPDATE OF active ON paperboat.team_resource_bindings
    FOR EACH ROW WHEN (NEW.resource_kind = 'machine')
    EXECUTE FUNCTION paperboat.publish_machine_binding_recording_policy();

-- +goose Down
DROP TRIGGER team_resource_bindings_publish_recording_policy ON paperboat.team_resource_bindings;
DROP FUNCTION paperboat.publish_machine_binding_recording_policy();
