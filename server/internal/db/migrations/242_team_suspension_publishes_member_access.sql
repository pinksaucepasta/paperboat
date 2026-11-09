-- +goose Up
-- A team's suspension or deletion ends every member's access to its machines (team_paid_entitled), but migration 196
-- published only the hosts' topics, so a member's signed peer-network document kept the team machines' dial scopes
-- until the inventory fallback. It now also moves the active members' inventory (peer-network renewal) and
-- machine_access (fast machine-list refresh) account topics, once per statement over the teams whose suspended or
-- deleted_at changed (as migrations 193 and 235).
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_team_suspension_members() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE members text[];
BEGIN
  members := ARRAY(SELECT DISTINCT m.account_id FROM paperboat.team_members m
    WHERE m.active AND m.team_id IN (SELECT n.team_id FROM new_teams n JOIN old_teams o ON o.team_id = n.team_id
      WHERE o.suspended IS DISTINCT FROM n.suspended OR o.deleted_at IS DISTINCT FROM n.deleted_at));
  PERFORM paperboat.publish_account_topic(members, 'inventory');
  PERFORM paperboat.publish_account_topic(members, 'machine_access');
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER teams_publish_suspension_members AFTER UPDATE ON paperboat.teams
    REFERENCING OLD TABLE AS old_teams NEW TABLE AS new_teams
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_team_suspension_members();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER teams_publish_suspension_members ON paperboat.teams;
DROP FUNCTION paperboat.publish_team_suspension_members();
