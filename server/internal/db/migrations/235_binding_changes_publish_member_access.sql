-- +goose Up
-- Sharing or unsharing a machine changes what every team member's own devices may dial, but migration 196 published
-- only the HOST's peer_network and machine_access topics. A member's signed peer-network document kept the unshared
-- machine's machine_access scope until the inventory fallback (5 minutes). A binding change now also moves the
-- members' inventory (wakes the daemon's peer-network renewal) and machine_access (fast machine-list refresh) account
-- topics, once per statement over the affected teams (as 193), and only for machine bindings and real changes.
-- Prior art: migrations 207 and 209 (same topics for a policy or private access change).
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_binding_members() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  changed_teams text[];
  members text[];
BEGIN
  IF TG_OP = 'INSERT' THEN
    changed_teams := ARRAY(SELECT DISTINCT team_id FROM new_bindings WHERE resource_kind = 'machine');
  ELSIF TG_OP = 'DELETE' THEN
    changed_teams := ARRAY(SELECT DISTINCT team_id FROM old_bindings WHERE resource_kind = 'machine');
  ELSE
    changed_teams := ARRAY(SELECT DISTINCT n.team_id FROM new_bindings n
      JOIN old_bindings o ON o.team_id = n.team_id AND o.resource_kind = n.resource_kind AND o.resource_id = n.resource_id
      WHERE n.resource_kind = 'machine' AND (o.active IS DISTINCT FROM n.active OR o.generation IS DISTINCT FROM n.generation));
  END IF;
  IF cardinality(changed_teams) = 0 THEN
    RETURN NULL;
  END IF;
  members := ARRAY(SELECT DISTINCT m.account_id FROM paperboat.team_members m WHERE m.team_id = ANY (changed_teams) AND m.active);
  PERFORM paperboat.publish_account_topic(members, 'inventory');
  PERFORM paperboat.publish_account_topic(members, 'machine_access');
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER team_resource_bindings_publish_members_insert AFTER INSERT ON paperboat.team_resource_bindings
    REFERENCING NEW TABLE AS new_bindings
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_binding_members();
CREATE TRIGGER team_resource_bindings_publish_members_update AFTER UPDATE ON paperboat.team_resource_bindings
    REFERENCING OLD TABLE AS old_bindings NEW TABLE AS new_bindings
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_binding_members();
CREATE TRIGGER team_resource_bindings_publish_members_delete AFTER DELETE ON paperboat.team_resource_bindings
    REFERENCING OLD TABLE AS old_bindings
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_binding_members();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER team_resource_bindings_publish_members_delete ON paperboat.team_resource_bindings;
DROP TRIGGER team_resource_bindings_publish_members_update ON paperboat.team_resource_bindings;
DROP TRIGGER team_resource_bindings_publish_members_insert ON paperboat.team_resource_bindings;
DROP FUNCTION paperboat.publish_binding_members();
