-- +goose Up
-- A new version of a team's policy may change who is recorded where.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_workspace_policy_recording_policy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.team_id IS NOT NULL THEN
    PERFORM paperboat.publish_team_recording_policy(ARRAY[NEW.team_id]);
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER workspace_policies_publish_recording_policy
    AFTER UPDATE OF version ON paperboat.workspace_policies
    FOR EACH ROW WHEN (old.version IS DISTINCT FROM new.version)
    EXECUTE FUNCTION paperboat.publish_workspace_policy_recording_policy();

-- +goose Down
DROP TRIGGER workspace_policies_publish_recording_policy ON paperboat.workspace_policies;
DROP FUNCTION paperboat.publish_workspace_policy_recording_policy();
