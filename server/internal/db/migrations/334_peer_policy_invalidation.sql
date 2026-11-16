-- +goose Up
SET LOCAL lock_timeout = '5s';
-- The server keeps each endpoint's peer policy document in memory (peersessions/policy_cache.go) and drops it when a
-- machine_events notification names one of its parties: its endpoint, its machine, its account, or a peer's. The
-- topic triggers already notify for most writes, but not for every input the document reads (grant and admission
-- rows on the client side, certificates, environments, terminal grants, policy rules, recording keys, ...). This is
-- the one owner of the rest: publish_peer_policy sends 'policy:<key>' notifications (no generation row is written,
-- and waiting hosts ignore the prefix), and every table the document reads gets a trigger that names the parties
-- the change can affect. A team change names the whole team: every member's account, its owner, and every machine
-- the team owns or is shared.
-- Prior art: headscale recompiles on every policy-relevant write (hscontrol/policy/v2/policy.go updateLocked); NetBird
-- bumps the account network serial on every write (management/server/types/account_components.go).

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_policy(keys text[]) RETURNS void LANGUAGE sql AS $$
 SELECT pg_notify('machine_events', 'policy:' || k) FROM (SELECT DISTINCT unnest(keys) AS k) x WHERE k IS NOT NULL AND k NOT IN ('', 'account:');
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.peer_policy_team_keys(teams text[]) RETURNS text[] LANGUAGE sql STABLE AS $$
 SELECT coalesce(array_agg(DISTINCT k), '{}'::text[]) FROM (
  SELECT 'account:' || m.account_id AS k FROM paperboat.team_members m WHERE m.team_id = ANY (teams)
  UNION SELECT 'account:' || t.owner_account FROM paperboat.teams t WHERE t.team_id = ANY (teams)
  UNION SELECT b.resource_id FROM paperboat.team_resource_bindings b WHERE b.team_id = ANY (teams) AND b.resource_kind = 'machine'
  UNION SELECT 'account:' || um.user_id FROM paperboat.team_resource_bindings b JOIN paperboat.user_machines um ON um.id = b.resource_id
   WHERE b.team_id = ANY (teams) AND b.resource_kind = 'machine'
  UNION SELECT um.id FROM paperboat.user_machines um WHERE um.owner_team_id = ANY (teams)
  UNION SELECT 'account:' || um.user_id FROM paperboat.user_machines um WHERE um.owner_team_id = ANY (teams)
 ) keys
$$;
-- +goose StatementEnd

-- Tables keyed by a team (statement level: a bulk change names each team once).
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_policy_teams() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_team_keys(ARRAY(SELECT DISTINCT team_id FROM new_rows)));
  ELSIF TG_OP = 'DELETE' THEN
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_team_keys(ARRAY(SELECT DISTINCT team_id FROM old_rows)));
  ELSE
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_team_keys(ARRAY(SELECT team_id FROM new_rows UNION SELECT team_id FROM old_rows)));
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- One team row (a deleted team's members and machines are still named in its AFTER ROW trigger) or one membership row.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_policy_team_row() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_TABLE_NAME = 'teams' THEN
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_team_keys(ARRAY[coalesce(NEW.team_id, OLD.team_id)]) || ARRAY['account:' || OLD.owner_account, 'account:' || NEW.owner_account]);
  ELSE -- team_members: a member leaving or moving is no longer in the team's own rows
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_team_keys(ARRAY[NEW.team_id, OLD.team_id]) || ARRAY['account:' || OLD.account_id, 'account:' || NEW.account_id]);
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- Workspace policies: a team policy names its team, a personal one its owner.
-- +goose StatementBegin
CREATE FUNCTION paperboat.peer_policy_workspace_keys(policies bigint[]) RETURNS text[] LANGUAGE sql STABLE AS $$
 SELECT paperboat.peer_policy_team_keys(ARRAY(SELECT p.team_id FROM paperboat.workspace_policies p WHERE p.id = ANY (policies) AND p.team_id IS NOT NULL))
  || ARRAY(SELECT 'account:' || p.owner_account FROM paperboat.workspace_policies p WHERE p.id = ANY (policies) AND p.owner_account IS NOT NULL)
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_policy_workspace() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_workspace_keys(ARRAY(SELECT DISTINCT policy_id FROM new_rows)));
  ELSIF TG_OP = 'DELETE' THEN
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_workspace_keys(ARRAY(SELECT DISTINCT policy_id FROM old_rows)));
  ELSE
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_workspace_keys(ARRAY(SELECT policy_id FROM new_rows UNION SELECT policy_id FROM old_rows)));
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_policy_workspace_row() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_peer_policy(
    CASE WHEN coalesce(NEW.team_id, OLD.team_id) IS NOT NULL THEN paperboat.peer_policy_team_keys(ARRAY[NEW.team_id, OLD.team_id]) ELSE '{}'::text[] END
    || ARRAY['account:' || NEW.owner_account, 'account:' || OLD.owner_account]);
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- Rows of one account, row level with a WHEN on the columns the document reads (frequent writes of other columns,
-- like a configuration request's own generation bump, notify nothing).
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_policy_account_row() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE r record;
BEGIN
  IF TG_OP = 'DELETE' THEN r := OLD; ELSE r := NEW; END IF;
  CASE TG_TABLE_NAME
  WHEN 'peer_network_identities' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY['account:' || r.user_id, r.endpoint_id, r.machine_id]);
  WHEN 'peer_endpoint_certificates' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY['account:' || r.user_id, r.endpoint_id]);
  WHEN 'cli_client_sessions' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY['account:' || r.user_id, r.id]);
  WHEN 'users' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY['account:' || r.id]);
  WHEN 'control_environments' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY['account:' || r.owner_user_id]
      || CASE WHEN TG_OP = 'UPDATE' THEN ARRAY['account:' || OLD.owner_user_id] ELSE '{}'::text[] END);
  WHEN 'user_machine_terminal_sessions' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY[r.user_machine_id, 'account:' || r.owner_account]
      || ARRAY(SELECT 'account:' || s.user_id FROM paperboat.user_machine_access_sessions s WHERE s.terminal_session_id = r.id AND s.state = 'active'));
  WHEN 'control_config_assignments', 'control_config_sync_statuses' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY[r.machine_id]
      || ARRAY(SELECT 'account:' || e.owner_user_id FROM paperboat.control_environments e WHERE e.id = r.environment_id));
  WHEN 'billing_workspaces' THEN -- a team's paid entitlement (team_paid_entitled); personal relay quota is read per request
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_team_keys(ARRAY(SELECT x FROM unnest(ARRAY[r.team_id, CASE WHEN TG_OP = 'UPDATE' THEN OLD.team_id END]) x WHERE x IS NOT NULL)));
  WHEN 'recording_keys' THEN
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_team_keys(ARRAY[r.team_id]));
  -- Grants and admissions name the client endpoint and the machine, before and after; not their accounts, so an exec
  -- does not drop every other document of the account. A lazy policy has no client endpoint: its account.
  WHEN 'user_machine_access_sessions' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY[r.user_machine_id, r.cli_client_session_id]
      || CASE WHEN TG_OP = 'UPDATE' THEN ARRAY[OLD.user_machine_id, OLD.cli_client_session_id] ELSE '{}'::text[] END);
  WHEN 'inspector_credentials' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY[r.native_machine_id, r.native_cli_session_id]
      || CASE WHEN TG_OP = 'UPDATE' THEN ARRAY[OLD.native_machine_id, OLD.native_cli_session_id] ELSE '{}'::text[] END);
  WHEN 'config_compare_admissions', 'recording_native_admissions' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY[r.machine_id, r.native_cli_session_id]
      || CASE WHEN TG_OP = 'UPDATE' THEN ARRAY[OLD.machine_id, OLD.native_cli_session_id] ELSE '{}'::text[] END);
  WHEN 'lazy_access_policies' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY['account:' || r.account_id, r.machine_id]
      || CASE WHEN TG_OP = 'UPDATE' THEN ARRAY['account:' || OLD.account_id, OLD.machine_id] ELSE '{}'::text[] END);
  END CASE;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- A machine: its own id and account, and its team when it is team-owned. A presence-only change (online, or state
-- between online and offline) reaches only the machine and the accounts holding a terminal grant on it (the one
-- part of a document that reads presence), so a flapping machine does not drop every document of its account.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_policy_machine() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE r record;
BEGIN
  IF TG_OP = 'DELETE' THEN r := OLD; ELSE r := NEW; END IF;
  IF TG_OP = 'UPDATE'
     AND (OLD.seat_state, OLD.revoked_at, OLD.deleted_at, OLD.installation_generation, OLD.environment_id, OLD.owner_team_id, OLD.user_id, OLD.configured_capabilities, OLD.observed_capabilities)
         IS NOT DISTINCT FROM (NEW.seat_state, NEW.revoked_at, NEW.deleted_at, NEW.installation_generation, NEW.environment_id, NEW.owner_team_id, NEW.user_id, NEW.configured_capabilities, NEW.observed_capabilities)
     AND OLD.state IN ('online', 'offline') AND NEW.state IN ('online', 'offline') THEN
    PERFORM paperboat.publish_peer_policy(ARRAY[NEW.id]
      || ARRAY(SELECT 'account:' || s.user_id FROM paperboat.user_machine_access_sessions s
               WHERE s.user_machine_id = NEW.id AND s.state = 'active' AND s.terminal_role IN ('viewer', 'interactive')));
    RETURN NULL;
  END IF;
  PERFORM paperboat.publish_peer_policy(ARRAY[r.id, 'account:' || r.user_id]
    || CASE WHEN TG_OP = 'UPDATE' THEN ARRAY['account:' || OLD.user_id] ELSE '{}'::text[] END
    || paperboat.peer_policy_team_keys(ARRAY(SELECT x FROM unnest(ARRAY[r.owner_team_id, CASE WHEN TG_OP = 'UPDATE' THEN OLD.owner_team_id END]) x WHERE x IS NOT NULL)));
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- One migration per table (the triggers below and in 336 onward): CREATE TRIGGER holds SHARE ROW EXCLUSIVE on its table until
-- commit, so each table is its own short transaction. This one: the functions (no table locks) and team_members.
CREATE TRIGGER peer_policy_insert AFTER INSERT ON paperboat.team_members REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_peer_policy_teams();
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.team_members FOR EACH ROW
 WHEN ((OLD.team_id, OLD.account_id, OLD.active, OLD.role, OLD.membership_generation) IS DISTINCT FROM (NEW.team_id, NEW.account_id, NEW.active, NEW.role, NEW.membership_generation))
 EXECUTE FUNCTION paperboat.publish_peer_policy_team_row();
CREATE TRIGGER peer_policy_delete AFTER DELETE ON paperboat.team_members REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_peer_policy_teams();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy_insert ON paperboat.team_members;
DROP TRIGGER peer_policy_update ON paperboat.team_members;
DROP TRIGGER peer_policy_delete ON paperboat.team_members;
DROP FUNCTION paperboat.publish_peer_policy_machine();
DROP FUNCTION paperboat.publish_peer_policy_account_row();
DROP FUNCTION paperboat.publish_peer_policy_workspace_row();
DROP FUNCTION paperboat.publish_peer_policy_workspace();
DROP FUNCTION paperboat.peer_policy_workspace_keys(bigint[]);
DROP FUNCTION paperboat.publish_peer_policy_team_row();
DROP FUNCTION paperboat.publish_peer_policy_teams();
DROP FUNCTION paperboat.peer_policy_team_keys(text[]);
DROP FUNCTION paperboat.publish_peer_policy(text[]);
