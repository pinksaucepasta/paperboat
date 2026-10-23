-- +goose Up
-- machine_audience_accounts (every topic publish for a machine: inventory, previews, machine services, peer network)
-- asked the policy seven times per member of every team the machine is shared into, one workspace_policy_ports
-- call each (not inlined, planned per call, 0.7 ms): 300 ms for a 60-member team, inside the transaction of every
-- preview create or stop and machine change on it. workspace_policy_audience evaluates the same rules for all
-- members and capabilities in one query (TestPolicySQLMatchesGo holds it to workspace_policy_ports).
-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_policy_audience(team text, machine text) RETURNS SETOF text
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 WITH p AS (SELECT id, allow_private_access FROM workspace_policies WHERE team_id = team),
 caps(action) AS (SELECT unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage','private_access'])),
 targeted AS (
  SELECT r.* FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id JOIN caps USING (action)
  WHERE r.target_kind = 'machine' AND r.target_id = machine
  UNION ALL
  SELECT r.* FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id JOIN caps USING (action)
  WHERE r.target_kind = 'all_machines'
  UNION ALL
  SELECT r.* FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id JOIN caps USING (action)
  WHERE r.target_kind = 'tag'
   AND EXISTS(SELECT 1 FROM workspace_policy_tags tg WHERE tg.policy_id = p.id AND tg.tag = r.target_id AND tg.machine_id = machine)
 ), members AS (
  SELECT member.account_id, CASE WHEN t.owner_account = member.account_id THEN 'owner' ELSE member.role END AS role
  FROM teams t JOIN team_members member USING (team_id)
  WHERE t.team_id = team AND member.active
 ), matching AS (
  SELECT a.account_id, r.action, r.deny, r.ports FROM targeted r JOIN members a ON
      r.subject_kind = 'all'
   OR (r.subject_kind = 'account' AND r.subject_id = a.account_id)
   OR (r.subject_kind = 'role' AND (r.subject_id = 'member' OR r.subject_id = a.role OR (r.subject_id = 'admin' AND a.role = 'owner')))
   OR (r.subject_kind = 'group' AND EXISTS(SELECT 1 FROM workspace_policy_groups g WHERE g.policy_id = r.policy_id AND g.group_name = r.subject_id AND g.account_id = a.account_id))
 ), granted AS (
  SELECT account_id, action,
   coalesce(range_agg(ports) FILTER (WHERE NOT deny), '{}'::int4multirange)
    - coalesce(range_agg(ports) FILTER (WHERE deny), '{}'::int4multirange) AS ports
  FROM matching GROUP BY account_id, action
 )
 SELECT DISTINCT account_id FROM granted
 WHERE NOT isempty(ports) AND (action <> 'private_access' OR coalesce((SELECT allow_private_access FROM p), false));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.machine_audience_accounts(wanted_machine text) RETURNS text[]
    LANGUAGE sql STABLE AS $$
 SELECT coalesce(array_agg(DISTINCT audience.account_id), '{}'::text[]) FROM (
  SELECT own.user_id AS account_id FROM paperboat.user_machines own WHERE own.id = wanted_machine
  UNION
  SELECT member.account_id FROM paperboat.user_machines own
   JOIN paperboat.team_members member ON member.team_id = own.owner_team_id AND member.active
   WHERE own.id = wanted_machine AND own.owner_team_id IS NOT NULL
  UNION
  SELECT allowed.account_id FROM paperboat.team_resource_bindings b
   CROSS JOIN LATERAL paperboat.workspace_policy_audience(b.team_id, wanted_machine) AS allowed(account_id)
   WHERE b.resource_kind = 'machine' AND b.resource_id = wanted_machine AND b.active
 ) audience
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.machine_audience_accounts(wanted_machine text) RETURNS text[]
    LANGUAGE sql STABLE AS $$
 SELECT coalesce(array_agg(DISTINCT audience.account_id), '{}'::text[]) FROM (
  SELECT own.user_id AS account_id FROM paperboat.user_machines own WHERE own.id = wanted_machine
  UNION
  SELECT member.account_id FROM paperboat.user_machines own
   JOIN paperboat.team_members member ON member.team_id = own.owner_team_id AND member.active
   WHERE own.id = wanted_machine AND own.owner_team_id IS NOT NULL
  UNION
  SELECT member.account_id FROM paperboat.team_resource_bindings b
   JOIN paperboat.team_members member ON member.team_id = b.team_id AND member.active
   WHERE b.resource_kind = 'machine' AND b.resource_id = wanted_machine AND b.active
    AND EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage','private_access']) cap
      WHERE NOT isempty(paperboat.workspace_policy_ports(member.account_id, b.team_id, wanted_machine, cap)))
 ) audience
$$;
-- +goose StatementEnd
DROP FUNCTION paperboat.workspace_policy_audience(text, text);
