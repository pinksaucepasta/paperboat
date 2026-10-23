-- +goose Up
-- workspace_policy_rule_ports scanned every rule of the team's policy for the capability on each call: its target
-- test (all_machines OR this machine OR a tag holding it) is one OR no index serves. Per-machine rules are the
-- common shape (migration 128 made one per old grant), so a machine list cost machines x rules (0.7 ms per call
-- with 1500 rules, seven calls per machine). Each target kind is now its own indexed branch; the evaluation is
-- unchanged (shared/policy Compiled.Decide, TestPolicySQLMatchesGo).
CREATE INDEX workspace_policy_rules_target ON paperboat.workspace_policy_rules(policy_id, action, target_kind, target_id);
DROP INDEX paperboat.workspace_policy_rules_lookup; -- a prefix of the new index

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.workspace_policy_rule_ports(actor text, team text, machine text, capability text) RETURNS int4multirange
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 WITH p AS (SELECT id FROM workspace_policies WHERE team_id = team),
 actor_role AS (
  SELECT CASE WHEN t.owner_account = actor THEN 'owner' ELSE member.role END AS role
  FROM teams t JOIN team_members member USING (team_id)
  WHERE t.team_id = team AND member.account_id = actor AND member.active
 ), targeted AS (
  SELECT r.* FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id
  WHERE r.action = capability AND r.target_kind = 'machine' AND r.target_id = machine
  UNION ALL
  SELECT r.* FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id
  WHERE r.action = capability AND r.target_kind = 'all_machines'
  UNION ALL
  SELECT r.* FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id
  WHERE r.action = capability AND r.target_kind = 'tag'
   AND EXISTS(SELECT 1 FROM workspace_policy_tags tg WHERE tg.policy_id = p.id AND tg.tag = r.target_id AND tg.machine_id = machine)
 ), matching AS (
  SELECT r.deny, r.ports FROM targeted r CROSS JOIN actor_role a
  WHERE r.subject_kind = 'all'
     OR (r.subject_kind = 'account' AND r.subject_id = actor)
     OR (r.subject_kind = 'role' AND (r.subject_id = 'member' OR r.subject_id = a.role OR (r.subject_id = 'admin' AND a.role = 'owner')))
     OR (r.subject_kind = 'group' AND EXISTS(SELECT 1 FROM workspace_policy_groups g WHERE g.policy_id = r.policy_id AND g.group_name = r.subject_id AND g.account_id = actor))
 )
 SELECT coalesce((SELECT range_agg(ports) FROM matching WHERE NOT deny), '{}'::int4multirange)
      - coalesce((SELECT range_agg(ports) FROM matching WHERE deny), '{}'::int4multirange);
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.workspace_policy_rule_ports(actor text, team text, machine text, capability text) RETURNS int4multirange
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 WITH p AS (SELECT id FROM workspace_policies WHERE team_id = team),
 actor_role AS (
  SELECT CASE WHEN t.owner_account = actor THEN 'owner' ELSE member.role END AS role
  FROM teams t JOIN team_members member USING (team_id)
  WHERE t.team_id = team AND member.account_id = actor AND member.active
 ), matching AS (
  SELECT r.deny, r.ports FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id CROSS JOIN actor_role a
  WHERE r.action = capability
   AND (r.subject_kind = 'all'
     OR (r.subject_kind = 'account' AND r.subject_id = actor)
     OR (r.subject_kind = 'role' AND (r.subject_id = 'member' OR r.subject_id = a.role OR (r.subject_id = 'admin' AND a.role = 'owner')))
     OR (r.subject_kind = 'group' AND EXISTS(SELECT 1 FROM workspace_policy_groups g WHERE g.policy_id = p.id AND g.group_name = r.subject_id AND g.account_id = actor)))
   AND (r.target_kind = 'all_machines'
     OR (r.target_kind = 'machine' AND r.target_id = machine)
     OR (r.target_kind = 'tag' AND EXISTS(SELECT 1 FROM workspace_policy_tags tg WHERE tg.policy_id = p.id AND tg.tag = r.target_id AND tg.machine_id = machine)))
 )
 SELECT coalesce((SELECT range_agg(ports) FROM matching WHERE NOT deny), '{}'::int4multirange)
      - coalesce((SELECT range_agg(ports) FROM matching WHERE deny), '{}'::int4multirange);
$$;
-- +goose StatementEnd
CREATE INDEX workspace_policy_rules_lookup ON paperboat.workspace_policy_rules(policy_id, action);
DROP INDEX paperboat.workspace_policy_rules_target;
