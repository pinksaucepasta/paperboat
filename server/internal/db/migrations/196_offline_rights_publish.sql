-- +goose Up
-- F21-11 (PLAN 3b offline-capable authority, point 6): a host's peer document carries the sessions each peer may
-- start on it offline (peersessions offline_access.go). Every change that decides them must move the host's
-- peer_network topic, so the host refreshes its document instead of keeping a right that ended: a machine grant, a
-- team's suspension, a member joining or leaving, a team policy version, an account's status. Each trigger fires only
-- when its deciding column changes (one writer per column, no flapping), and publishes only to the hosts concerned.

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_team_hosts_peer_network(wanted_team text) RETURNS void
    LANGUAGE sql AS $$
 SELECT paperboat.publish_machine_topic(coalesce((SELECT array_agg(b.resource_id) FROM paperboat.team_resource_bindings b
  WHERE b.team_id = wanted_team AND b.resource_kind = 'machine' AND b.active), '{}'::text[]), 'peer_network');
$$;
-- +goose StatementEnd

-- A machine grant (who may start which session on it).
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_grant_host_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE grant_row paperboat.team_resource_grants;
BEGIN
  grant_row := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
  IF grant_row.resource_kind = 'machine' THEN
    PERFORM paperboat.publish_machine_topic(ARRAY[grant_row.resource_id], 'peer_network');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER team_resource_grants_publish_host_peer_network_insert
    AFTER INSERT OR DELETE ON paperboat.team_resource_grants
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_grant_host_peer_network();
CREATE TRIGGER team_resource_grants_publish_host_peer_network_update
    AFTER UPDATE OF active, permission, generation ON paperboat.team_resource_grants
    FOR EACH ROW WHEN (old.active IS DISTINCT FROM new.active OR old.permission IS DISTINCT FROM new.permission OR old.generation IS DISTINCT FROM new.generation)
    EXECUTE FUNCTION paperboat.publish_grant_host_peer_network();

-- A machine shared into or out of a team: every member's offline rights on that host start or end (the binding's
-- generation is part of the scope's generation).
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_binding_host_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE binding_row paperboat.team_resource_bindings;
BEGIN
  binding_row := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
  IF binding_row.resource_kind = 'machine' THEN
    PERFORM paperboat.publish_machine_topic(ARRAY[binding_row.resource_id], 'peer_network');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER team_resource_bindings_publish_host_peer_network_insert
    AFTER INSERT OR DELETE ON paperboat.team_resource_bindings
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_binding_host_peer_network();
CREATE TRIGGER team_resource_bindings_publish_host_peer_network_update
    AFTER UPDATE OF active, generation ON paperboat.team_resource_bindings
    FOR EACH ROW WHEN (old.active IS DISTINCT FROM new.active OR old.generation IS DISTINCT FROM new.generation)
    EXECUTE FUNCTION paperboat.publish_binding_host_peer_network();

-- A team's suspension (its members' rights on its machines).
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_team_suspension_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_team_hosts_peer_network(NEW.team_id);
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER teams_publish_suspension_peer_network
    AFTER UPDATE OF suspended, deleted_at ON paperboat.teams
    FOR EACH ROW WHEN (old.suspended IS DISTINCT FROM new.suspended OR old.deleted_at IS DISTINCT FROM new.deleted_at)
    EXECUTE FUNCTION paperboat.publish_team_suspension_peer_network();

-- A member joining, leaving or re-joining (the membership generation is part of the scope's generation).
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_team_member_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_team_hosts_peer_network(CASE WHEN TG_OP = 'DELETE' THEN OLD.team_id ELSE NEW.team_id END);
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER team_members_publish_peer_network_insert
    AFTER INSERT OR DELETE ON paperboat.team_members
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_team_member_peer_network();
CREATE TRIGGER team_members_publish_peer_network_update
    AFTER UPDATE OF active, membership_generation ON paperboat.team_members
    FOR EACH ROW WHEN (old.active IS DISTINCT FROM new.active OR old.membership_generation IS DISTINCT FROM new.membership_generation)
    EXECUTE FUNCTION paperboat.publish_team_member_peer_network();

-- A team's policy version (port and tag rules decide machine access too).
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_team_policy_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.team_id IS NOT NULL THEN
    PERFORM paperboat.publish_team_hosts_peer_network(NEW.team_id);
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER workspace_policies_publish_peer_network
    AFTER UPDATE OF version ON paperboat.workspace_policies
    FOR EACH ROW WHEN (old.version IS DISTINCT FROM new.version)
    EXECUTE FUNCTION paperboat.publish_team_policy_peer_network();

-- An account's status: its own machines (account topic) and the machines of every team it belongs to.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_account_status_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE team_row record;
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY[NEW.id], 'peer_network');
  FOR team_row IN SELECT DISTINCT member.team_id FROM paperboat.team_members member WHERE member.account_id = NEW.id AND member.active LOOP
    PERFORM paperboat.publish_team_hosts_peer_network(team_row.team_id);
  END LOOP;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER users_publish_status_peer_network
    AFTER UPDATE OF status ON paperboat.users
    FOR EACH ROW WHEN (old.status IS DISTINCT FROM new.status)
    EXECUTE FUNCTION paperboat.publish_account_status_peer_network();

-- +goose Down
DROP TRIGGER team_resource_bindings_publish_host_peer_network_update ON paperboat.team_resource_bindings;
DROP TRIGGER team_resource_bindings_publish_host_peer_network_insert ON paperboat.team_resource_bindings;
DROP FUNCTION paperboat.publish_binding_host_peer_network();
DROP TRIGGER users_publish_status_peer_network ON paperboat.users;
DROP FUNCTION paperboat.publish_account_status_peer_network();
DROP TRIGGER workspace_policies_publish_peer_network ON paperboat.workspace_policies;
DROP FUNCTION paperboat.publish_team_policy_peer_network();
DROP TRIGGER team_members_publish_peer_network_update ON paperboat.team_members;
DROP TRIGGER team_members_publish_peer_network_insert ON paperboat.team_members;
DROP FUNCTION paperboat.publish_team_member_peer_network();
DROP TRIGGER teams_publish_suspension_peer_network ON paperboat.teams;
DROP FUNCTION paperboat.publish_team_suspension_peer_network();
DROP TRIGGER team_resource_grants_publish_host_peer_network_update ON paperboat.team_resource_grants;
DROP TRIGGER team_resource_grants_publish_host_peer_network_insert ON paperboat.team_resource_grants;
DROP FUNCTION paperboat.publish_grant_host_peer_network();
DROP FUNCTION paperboat.publish_team_hosts_peer_network(text);
