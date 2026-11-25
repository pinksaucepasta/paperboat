-- +goose Up
-- A SQL function plans its body without its arguments; PL/pgSQL plans it for the call's (total of 12,000 previews
-- 423 ms -> 35). The per-row rules could not be inlined (SET search_path, an EXISTS): the same rules without SET and
-- with the EXISTS in its own function are (a team member's total of 1,500 previews 180 ms -> 12). Candidates are
-- limited to the requested, present machines first, and carry only the columns the rules read. use_column is how SQL
-- reads a name that is both a column and an argument.
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE FUNCTION paperboat.preview_bound_to_team(team text, preview_id text, preview_account text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM team_resource_bindings b WHERE b.team_id=team
  AND b.resource_kind='preview' AND b.resource_id=preview_id AND b.owner_account=preview_account AND b.active);
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.workspace_preview_management_decision(actor text, team text, preview_id text, preview_account text,
    preview_team text, access_mode text, host_user text, host_team text, host_capabilities text[], host_manage boolean) RETURNS boolean
LANGUAGE sql STABLE AS $$
 SELECT (CASE WHEN team IS NULL THEN preview_team IS NULL AND preview_account=actor AND host_team IS NULL AND host_user=actor
  ELSE (host_capabilities @> ARRAY['preview_launch']::text[] OR (preview_account=actor AND host_user=actor AND host_team IS NULL))
   AND (access_mode IN ('private','team') OR (preview_account=actor AND host_user=actor AND host_team IS NULL))
   AND (preview_team=team OR (preview_team IS NULL AND paperboat.preview_bound_to_team(team, preview_id, preview_account))) END)
  AND (team IS NULL OR coalesce(host_manage, false));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.preview_management_decision(actor text, preview_id text, preview_account text, preview_team text,
    access_mode text, host_user text, host_team text, host_capabilities text[], team_host_manage boolean, host_manage boolean) RETURNS boolean
LANGUAGE sql STABLE AS $$
 SELECT (preview_team IS NOT NULL AND paperboat.workspace_preview_management_decision(actor, preview_team, preview_id, preview_account, preview_team,
   access_mode, host_user, host_team, host_capabilities, team_host_manage))
  OR (preview_team IS NULL AND ((preview_account=actor AND host_team IS NULL AND host_user=actor)
   OR (access_mode IN ('private','team') AND host_capabilities @> ARRAY['preview_launch']::text[] AND coalesce(host_manage, false))));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.manageable_preview_ids(actor text, workspace_set boolean, workspace_team text, q text, state text, owner text,
    machine text, after_created_at timestamptz, after_id text, row_limit integer) RETURNS SETOF text
LANGUAGE plpgsql STABLE SET search_path TO 'paperboat','pg_catalog' SET plan_cache_mode TO 'force_custom_plan' AS $$
#variable_conflict use_column
BEGIN RETURN QUERY
 WITH rules AS (
   -- The allow rules for preview_manage in the account's teams whose subject may be the account (any role: a superset).
   SELECT tm.team_id, r.policy_id, r.target_kind, r.target_id FROM team_members AS tm
   JOIN workspace_policies AS p ON p.team_id = tm.team_id
   JOIN workspace_policy_rules AS r ON r.policy_id = p.id AND r.action = 'preview_manage' AND NOT r.deny
   WHERE tm.account_id = actor AND tm.active
     AND (r.subject_kind IN ('all', 'role') OR (r.subject_kind = 'account' AND r.subject_id = actor)
       OR (r.subject_kind = 'group' AND EXISTS(SELECT 1 FROM workspace_policy_groups AS g
         WHERE g.policy_id = r.policy_id AND g.group_name = r.subject_id AND g.account_id = actor)))
 ), targets AS (
   SELECT r.team_id, r.target_id AS machine_id FROM rules AS r WHERE r.target_kind = 'machine'
   UNION
   SELECT r.team_id, tg.machine_id FROM rules AS r
   JOIN workspace_policy_tags AS tg ON tg.policy_id = r.policy_id AND tg.tag = r.target_id WHERE r.target_kind = 'tag'
 ), hosts AS (
   SELECT m.id FROM user_machines AS m WHERE m.user_id = actor AND m.owner_team_id IS NULL
   UNION
   SELECT b.resource_id FROM team_resource_bindings AS b JOIN targets AS t ON t.team_id = b.team_id AND t.machine_id = b.resource_id
   WHERE b.resource_kind = 'machine' AND b.active
   UNION
   SELECT b.resource_id FROM team_resource_bindings AS b JOIN rules AS r ON r.team_id = b.team_id AND r.target_kind = 'all_machines'
   WHERE b.resource_kind = 'machine' AND b.active
 ), candidates AS MATERIALIZED (
  SELECT p.id, p.account_id, p.owner_team_id, p.owner_machine_id, p.access_mode, p.created_at FROM preview_leases AS p
  WHERE p.owner_machine_id IN (SELECT m.id FROM hosts JOIN user_machines AS m ON m.id = hosts.id
    WHERE m.deleted_at IS NULL AND (machine='' OR m.id=machine))
    AND (p.account_id = actor OR p.access_mode IN ('private', 'team'))
    AND (q='' OR position(lower(q) in lower(p.id||' '||p.endpoint||' '||p.owner_machine_id))>0)
    AND (state='' OR CASE WHEN p.terminal_state='owner_lost' THEN 'owner_disconnected' WHEN p.terminal_state='expired' THEN 'expired' WHEN p.terminal_state IN ('stopped','failed') THEN 'stopped' WHEN p.allocation_state<>'ready' THEN 'allocating' WHEN p.edge_state<>'ready' OR p.origin_state<>'ready' THEN 'connecting' ELSE 'ready' END=state OR (state='active' AND p.terminal_state='active')
      OR (state='ended' AND p.terminal_state<>'active'))
    AND (owner='' OR (owner='mine' AND p.account_id=actor) OR (owner='shared' AND p.account_id<>actor) OR p.account_id=owner)
    AND (after_created_at IS NULL OR (p.created_at, p.id) < (after_created_at, after_id))
 ), used AS MATERIALIZED (
  SELECT m.* FROM user_machines AS m WHERE m.deleted_at IS NULL AND m.id IN (SELECT c.owner_machine_id FROM candidates AS c)
 ), host_rights AS MATERIALIZED (
  SELECT m.id, machine_capability_allowed(actor, m.id, 'preview_manage') AS manage FROM used AS m
 ), team_rights AS MATERIALIZED (
  SELECT m.id, t.team_id, workspace_publisher_host_capability_allowed(actor, t.team_id, m.id, 'preview_manage') AS manage
  FROM used AS m CROSS JOIN (SELECT c.owner_team_id AS team_id FROM candidates AS c WHERE c.owner_team_id IS NOT NULL
   UNION SELECT workspace_team WHERE workspace_team IS NOT NULL) AS t
 )
 -- Newest first, decided one row at a time until the page is full (OFFSET 0 keeps the order below the filter).
 SELECT x.id FROM (
  SELECT c.id, c.account_id, c.owner_team_id, c.access_mode, m.user_id AS host_user, m.owner_team_id AS host_team,
   m.configured_capabilities AS host_capabilities, own.manage AS own_manage, h.manage AS host_manage, ws.manage AS ws_manage
  FROM candidates AS c JOIN used AS m ON m.id = c.owner_machine_id
   LEFT JOIN host_rights AS h ON h.id = m.id
   LEFT JOIN team_rights AS own ON own.id = m.id AND own.team_id = c.owner_team_id
   LEFT JOIN team_rights AS ws ON ws.id = m.id AND ws.team_id = workspace_team
  ORDER BY c.created_at DESC, c.id DESC OFFSET 0
 ) AS x
 WHERE preview_management_decision(actor, x.id, x.account_id, x.owner_team_id, x.access_mode, x.host_user, x.host_team,
   x.host_capabilities, x.own_manage, x.host_manage)
  AND (NOT workspace_set OR workspace_preview_management_decision(actor, workspace_team, x.id, x.account_id, x.owner_team_id,
   x.access_mode, x.host_user, x.host_team, x.host_capabilities, x.ws_manage))
 LIMIT row_limit;
END;
$$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.manageable_preview_ids(actor text, workspace_set boolean, workspace_team text, q text, state text, owner text,
    machine text, after_created_at timestamptz, after_id text, row_limit integer) RETURNS SETOF text
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 WITH rules AS (
   -- The allow rules for preview_manage in the account's teams whose subject may be the account (any role: a superset).
   SELECT tm.team_id, r.policy_id, r.target_kind, r.target_id FROM team_members AS tm
   JOIN workspace_policies AS p ON p.team_id = tm.team_id
   JOIN workspace_policy_rules AS r ON r.policy_id = p.id AND r.action = 'preview_manage' AND NOT r.deny
   WHERE tm.account_id = actor AND tm.active
     AND (r.subject_kind IN ('all', 'role') OR (r.subject_kind = 'account' AND r.subject_id = actor)
       OR (r.subject_kind = 'group' AND EXISTS(SELECT 1 FROM workspace_policy_groups AS g
         WHERE g.policy_id = r.policy_id AND g.group_name = r.subject_id AND g.account_id = actor)))
 ), targets AS (
   SELECT r.team_id, r.target_id AS machine_id FROM rules AS r WHERE r.target_kind = 'machine'
   UNION
   SELECT r.team_id, tg.machine_id FROM rules AS r
   JOIN workspace_policy_tags AS tg ON tg.policy_id = r.policy_id AND tg.tag = r.target_id WHERE r.target_kind = 'tag'
 ), hosts AS (
   SELECT m.id FROM user_machines AS m WHERE m.user_id = actor AND m.owner_team_id IS NULL
   UNION
   SELECT b.resource_id FROM team_resource_bindings AS b JOIN targets AS t ON t.team_id = b.team_id AND t.machine_id = b.resource_id
   WHERE b.resource_kind = 'machine' AND b.active
   UNION
   SELECT b.resource_id FROM team_resource_bindings AS b JOIN rules AS r ON r.team_id = b.team_id AND r.target_kind = 'all_machines'
   WHERE b.resource_kind = 'machine' AND b.active
 ), candidates AS MATERIALIZED (
  SELECT p.* FROM preview_leases AS p
  WHERE p.owner_machine_id IN (SELECT hosts.id FROM hosts)
    AND (p.account_id = actor OR p.access_mode IN ('private', 'team'))
    AND (q='' OR position(lower(q) in lower(p.id||' '||p.endpoint||' '||p.owner_machine_id))>0)
    AND (state='' OR CASE WHEN p.terminal_state='owner_lost' THEN 'owner_disconnected' WHEN p.terminal_state='expired' THEN 'expired' WHEN p.terminal_state IN ('stopped','failed') THEN 'stopped' WHEN p.allocation_state<>'ready' THEN 'allocating' WHEN p.edge_state<>'ready' OR p.origin_state<>'ready' THEN 'connecting' ELSE 'ready' END=state OR (state='active' AND p.terminal_state='active')
      OR (state='ended' AND p.terminal_state<>'active'))
    AND (owner='' OR (owner='mine' AND p.account_id=actor) OR (owner='shared' AND p.account_id<>actor) OR p.account_id=owner)
    AND (machine='' OR p.owner_machine_id=machine)
    AND (after_created_at IS NULL OR (p.created_at, p.id) < (after_created_at, after_id))
 ), used AS MATERIALIZED (
  SELECT m.* FROM user_machines AS m WHERE m.deleted_at IS NULL AND m.id IN (SELECT c.owner_machine_id FROM candidates AS c)
 ), host_rights AS MATERIALIZED (
  SELECT m.id, machine_capability_allowed(actor, m.id, 'preview_manage') AS manage FROM used AS m
 ), team_rights AS MATERIALIZED (
  SELECT m.id, t.team_id, workspace_publisher_host_capability_allowed(actor, t.team_id, m.id, 'preview_manage') AS manage
  FROM used AS m CROSS JOIN (SELECT c.owner_team_id AS team_id FROM candidates AS c WHERE c.owner_team_id IS NOT NULL
   UNION SELECT workspace_team WHERE workspace_team IS NOT NULL) AS t
 )
 -- Newest first, decided one row at a time until the page is full (OFFSET 0 keeps the order below the filter).
 SELECT x.id FROM (
  SELECT c.id, c.account_id, c.owner_team_id, c.access_mode, m.user_id AS host_user, m.owner_team_id AS host_team,
   m.configured_capabilities AS host_capabilities, own.manage AS own_manage, h.manage AS host_manage, ws.manage AS ws_manage
  FROM candidates AS c JOIN used AS m ON m.id = c.owner_machine_id
   LEFT JOIN host_rights AS h ON h.id = m.id
   LEFT JOIN team_rights AS own ON own.id = m.id AND own.team_id = c.owner_team_id
   LEFT JOIN team_rights AS ws ON ws.id = m.id AND ws.team_id = workspace_team
  ORDER BY c.created_at DESC, c.id DESC OFFSET 0
 ) AS x
 WHERE preview_management_decision(actor, x.id, x.account_id, x.owner_team_id, x.access_mode, x.host_user, x.host_team,
   x.host_capabilities, x.own_manage, x.host_manage)
  AND (NOT workspace_set OR workspace_preview_management_decision(actor, workspace_team, x.id, x.account_id, x.owner_team_id,
   x.access_mode, x.host_user, x.host_team, x.host_capabilities, x.ws_manage))
 LIMIT row_limit;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.preview_management_decision(actor text, preview_id text, preview_account text, preview_team text,
    access_mode text, host_user text, host_team text, host_capabilities text[], team_host_manage boolean, host_manage boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT (preview_team IS NOT NULL AND workspace_preview_management_decision(actor, preview_team, preview_id, preview_account, preview_team,
   access_mode, host_user, host_team, host_capabilities, team_host_manage))
  OR (preview_team IS NULL AND ((preview_account=actor AND host_team IS NULL AND host_user=actor)
   OR (access_mode IN ('private','team') AND host_capabilities @> ARRAY['preview_launch']::text[] AND coalesce(host_manage, false))));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.workspace_preview_management_decision(actor text, team text, preview_id text, preview_account text,
    preview_team text, access_mode text, host_user text, host_team text, host_capabilities text[], host_manage boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT (CASE WHEN team IS NULL THEN preview_team IS NULL AND preview_account=actor AND host_team IS NULL AND host_user=actor
  ELSE (host_capabilities @> ARRAY['preview_launch']::text[] OR (preview_account=actor AND host_user=actor AND host_team IS NULL))
   AND (access_mode IN ('private','team') OR (preview_account=actor AND host_user=actor AND host_team IS NULL))
   AND (preview_team=team OR (preview_team IS NULL AND EXISTS(SELECT 1 FROM team_resource_bindings b WHERE b.team_id=team
    AND b.resource_kind='preview' AND b.resource_id=preview_id AND b.owner_account=preview_account AND b.active))) END)
  AND (team IS NULL OR coalesce(host_manage, false));
$$;
-- +goose StatementEnd

DROP FUNCTION paperboat.preview_bound_to_team(text, text, text);
