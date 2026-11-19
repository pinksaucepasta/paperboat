-- +goose Up
-- Statement triggers that matched old_* to new_* transition rows by key ran as nested loops: transition tables have no
-- statistics, so the planner expects one row and compares every new row with every old one (25M comparisons, ~7 s,
-- for 5k rows; prod 2026-11-19 on user_machine_access_sessions, fixed by 443). The changed rows are now
-- `new EXCEPT old` over the key and the compared columns (hashed or sorted, linear; EXCEPT treats NULLs as equal, as
-- IS DISTINCT FROM does). A row whose key changed now counts as changed too (the join skipped it).
-- Function bodies only (and machines_audience_accounts, the set form of machine_audience_accounts): no trigger,
-- table or data changes.
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_binding_members() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  changed_teams text[];
  members text[];
BEGIN
  IF TG_OP = 'INSERT' THEN
    changed_teams := ARRAY(SELECT DISTINCT team_id FROM new_bindings WHERE resource_kind = 'machine');
  ELSIF TG_OP = 'DELETE' THEN
    changed_teams := ARRAY(SELECT DISTINCT team_id FROM old_bindings WHERE resource_kind = 'machine');
  ELSE
    changed_teams := ARRAY(SELECT DISTINCT c.team_id FROM (
      SELECT team_id, resource_kind, resource_id, active, generation FROM new_bindings WHERE resource_kind = 'machine'
      EXCEPT SELECT team_id, resource_kind, resource_id, active, generation FROM old_bindings WHERE resource_kind = 'machine') c);
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_config_credential_revocations() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY(SELECT DISTINCT c.machine_id FROM (
    SELECT jti, machine_id, revoked_at FROM new_rows WHERE revoked_at IS NOT NULL
    EXCEPT SELECT jti, machine_id, revoked_at FROM old_rows) c), 'machine_access');
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The accounts that see any of the machines, in three set queries (machine_audience_accounts ran them once per
-- machine: a 20,000-machine statement took 6.8 s). machine_audience_accounts is the one-machine case of it.
CREATE FUNCTION paperboat.machines_audience_accounts(wanted text[]) RETURNS text[]
    LANGUAGE sql STABLE AS $$
 SELECT coalesce(array_agg(DISTINCT audience.account_id), '{}'::text[]) FROM (
  SELECT own.user_id AS account_id FROM paperboat.user_machines own WHERE own.id = ANY (wanted)
  UNION
  SELECT member.account_id FROM paperboat.user_machines own
   JOIN paperboat.team_members member ON member.team_id = own.owner_team_id AND member.active
   WHERE own.id = ANY (wanted) AND own.owner_team_id IS NOT NULL
  UNION
  SELECT allowed.account_id FROM paperboat.team_resource_bindings b
   CROSS JOIN LATERAL paperboat.workspace_policy_audience(b.team_id, b.resource_id) AS allowed(account_id)
   WHERE b.resource_kind = 'machine' AND b.resource_id = ANY (wanted) AND b.active
 ) audience
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.machine_audience_accounts(wanted_machine text) RETURNS text[]
    LANGUAGE sql STABLE AS $$ SELECT paperboat.machines_audience_accounts(ARRAY[wanted_machine]) $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_services_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  changed_machines text[];
BEGIN
  IF TG_OP = 'INSERT' THEN
    changed_machines := ARRAY(SELECT DISTINCT machine_id FROM new_snapshots);
  ELSE
    changed_machines := ARRAY(SELECT DISTINCT c.machine_id FROM (
      SELECT machine_id, services FROM new_snapshots EXCEPT SELECT machine_id, services FROM old_snapshots) c);
  END IF;
  IF cardinality(changed_machines) > 0 THEN
    PERFORM paperboat.publish_account_topic(paperboat.machines_audience_accounts(changed_machines), 'machine_services');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_private_routes_assignments() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT e.account_id FROM new_rows n JOIN paperboat.endpoints e ON e.id = n.endpoint_id), 'private_routes');
  ELSE
    PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT e.account_id FROM (
      SELECT DISTINCT c.endpoint_id FROM (
        SELECT route_id, edge_node_id, endpoint_id, desired, observed_ready, generation, carrier_session_id, edge_process_epoch FROM new_rows
        EXCEPT SELECT route_id, edge_node_id, endpoint_id, desired, observed_ready, generation, carrier_session_id, edge_process_epoch FROM old_rows) c) n
      JOIN paperboat.endpoints e ON e.id = n.endpoint_id), 'private_routes');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_private_routes_carriers() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT account_id FROM new_rows), 'private_routes');
  ELSE
    PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT c.account_id FROM (
      SELECT route_id, account_id, session_id, edge_process_epoch, admission - 'expires_at' AS admission FROM new_rows
      EXCEPT SELECT route_id, account_id, session_id, edge_process_epoch, admission - 'expires_at' FROM old_rows) c), 'private_routes');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_private_routes_endpoints() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT c.account_id FROM (
    SELECT id, account_id, desired_state, access_mode FROM new_rows
    EXCEPT SELECT id, account_id, desired_state, access_mode FROM old_rows) c), 'private_routes');
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_private_routes_leases() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT c.account_id FROM (
    SELECT id, account_id, terminal_state, access_mode FROM new_rows
    EXCEPT SELECT id, account_id, terminal_state, access_mode FROM old_rows) c), 'private_routes');
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_members_recording_policy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM paperboat.publish_team_recording_policy(ARRAY(SELECT DISTINCT team_id FROM new_members));
  ELSIF TG_OP = 'DELETE' THEN
    PERFORM paperboat.publish_team_recording_policy(ARRAY(SELECT DISTINCT team_id FROM old_members));
  ELSE
    PERFORM paperboat.publish_team_recording_policy(ARRAY(SELECT DISTINCT c.team_id FROM (
      SELECT team_id, account_id, active, role FROM new_members
      EXCEPT SELECT team_id, account_id, active, role FROM old_members) c));
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_memberships() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE changed text[];
BEGIN
  IF TG_OP = 'INSERT' THEN
    changed := ARRAY(SELECT DISTINCT account_id FROM new_members);
  ELSIF TG_OP = 'DELETE' THEN
    changed := ARRAY(SELECT DISTINCT account_id FROM old_members);
  ELSE
    changed := ARRAY(SELECT DISTINCT c.account_id FROM (
      SELECT team_id, account_id, active, role FROM new_members
      EXCEPT SELECT team_id, account_id, active, role FROM old_members) c);
  END IF;
  PERFORM paperboat.publish_account_topic(changed, 'inventory');
  PERFORM paperboat.publish_account_topic(changed, 'machine_access');
  IF TG_OP <> 'INSERT' THEN
    PERFORM paperboat.publish_machine_topic(ARRAY(SELECT DISTINCT h.machine_id FROM paperboat.managed_ssh_certificate_holders h
      WHERE h.user_id = ANY(changed)), 'machine_access');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_suspension_members() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE members text[];
BEGIN
  members := ARRAY(SELECT DISTINCT m.account_id FROM paperboat.team_members m
    WHERE m.active AND m.team_id IN (SELECT c.team_id FROM (
      SELECT team_id, suspended, deleted_at FROM new_teams EXCEPT SELECT team_id, suspended, deleted_at FROM old_teams) c));
  PERFORM paperboat.publish_account_topic(members, 'inventory');
  PERFORM paperboat.publish_account_topic(members, 'machine_access');
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION '447 replaced trigger function bodies only; restore them from migrations 193, 217, 235, 242, 264, 325, 332 if ever needed'; END $$;
-- +goose StatementEnd
