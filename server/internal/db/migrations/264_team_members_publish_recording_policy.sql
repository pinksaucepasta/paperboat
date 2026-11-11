-- +goose Up
-- A team's recording policy names who is recorded and who are admins, so a member joining, leaving or changing role
-- moves the recording_policy topic of the team's machines. Teams without a recording key publish nothing: their
-- machines record nothing whatever the members are. Statement trigger with transition tables (as 193).
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_team_recording_policy(wanted_teams text[]) RETURNS void
    LANGUAGE sql AS $$
 SELECT paperboat.publish_machine_topic(coalesce((SELECT array_agg(m.id) FROM paperboat.user_machines m
  WHERE m.owner_team_id = ANY(wanted_teams) AND m.deleted_at IS NULL AND m.revoked_at IS NULL
   AND EXISTS(SELECT 1 FROM paperboat.recording_keys k WHERE k.team_id = m.owner_team_id)), '{}'::text[]), 'recording_policy');
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_team_members_recording_policy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM paperboat.publish_team_recording_policy(ARRAY(SELECT DISTINCT team_id FROM new_members));
  ELSIF TG_OP = 'DELETE' THEN
    PERFORM paperboat.publish_team_recording_policy(ARRAY(SELECT DISTINCT team_id FROM old_members));
  ELSE
    PERFORM paperboat.publish_team_recording_policy(ARRAY(SELECT DISTINCT n.team_id FROM new_members n
      JOIN old_members o USING (team_id, account_id) WHERE o.active IS DISTINCT FROM n.active OR o.role IS DISTINCT FROM n.role));
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER team_members_publish_recording_policy_insert AFTER INSERT ON paperboat.team_members
    REFERENCING NEW TABLE AS new_members FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_team_members_recording_policy();
CREATE TRIGGER team_members_publish_recording_policy_update AFTER UPDATE ON paperboat.team_members
    REFERENCING OLD TABLE AS old_members NEW TABLE AS new_members FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_team_members_recording_policy();
CREATE TRIGGER team_members_publish_recording_policy_delete AFTER DELETE ON paperboat.team_members
    REFERENCING OLD TABLE AS old_members FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_team_members_recording_policy();

-- +goose Down
DROP TRIGGER team_members_publish_recording_policy_insert ON paperboat.team_members;
DROP TRIGGER team_members_publish_recording_policy_update ON paperboat.team_members;
DROP TRIGGER team_members_publish_recording_policy_delete ON paperboat.team_members;
DROP FUNCTION paperboat.publish_team_members_recording_policy();
DROP FUNCTION paperboat.publish_team_recording_policy(text[]);
