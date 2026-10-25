-- +goose Up
-- Since 174 every page of the preview and tunnel lists decided the account's whole manageable set before taking
-- its page, and the daemons' completion refresh pages through it about once a minute (prod: /v1/previews was the
-- largest database cost, ~600 ms a call). The set functions take the cursor and the page size: candidates newest
-- first, each decided only until the page is full; the host checks stay once per host. Only the total decides
-- every row. The 7-argument forms stay for servers still on 175 during the rollout, as calls of the new ones.
-- +goose StatementBegin
CREATE FUNCTION paperboat.manageable_preview_ids(actor text, workspace_set boolean, workspace_team text, q text, state text, owner text,
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
  SELECT m.* FROM user_machines AS m WHERE m.id IN (SELECT c.owner_machine_id FROM candidates AS c)
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
CREATE FUNCTION paperboat.manageable_tunnel_ids(actor text, workspace_set boolean, workspace_team text, q text, state text, owner text,
    machine text, after_created_at timestamptz, after_id text, row_limit integer) RETURNS SETOF text
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 WITH rules AS (
   -- The allow rules for tunnel_manage in the account's teams whose subject may be the account (any role: a superset).
   SELECT tm.team_id, r.policy_id, r.target_kind, r.target_id FROM team_members AS tm
   JOIN workspace_policies AS p ON p.team_id = tm.team_id
   JOIN workspace_policy_rules AS r ON r.policy_id = p.id AND r.action = 'tunnel_manage' AND NOT r.deny
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
  SELECT t.* FROM tunnels AS t
  WHERE t.created_by_host_id IN (SELECT hosts.id FROM hosts)
    AND (t.account_id = actor OR t.access_mode IN ('private', 'team'))
    AND (q='' OR position(lower(q) in lower(t.id||' '||t.name||' '||t.stable_endpoint||' '||t.created_by_host_id))>0)
    AND ((state='' AND t.desired_state<>'deleted') OR t.desired_state=state)
    AND (owner='' OR (owner='mine' AND t.account_id=actor) OR (owner='shared' AND t.account_id<>actor) OR t.account_id=owner)
    AND (machine='' OR t.created_by_host_id=machine OR EXISTS(SELECT 1 FROM tunnel_connectors AS c
      WHERE c.tunnel_id=t.id AND c.host_id=machine AND c.revoked_at IS NULL))
    AND (after_created_at IS NULL OR (t.created_at, t.id) < (after_created_at, after_id))
 ), connectors AS MATERIALIZED (
  SELECT c.tunnel_id, c.host_id FROM tunnel_connectors AS c WHERE c.tunnel_id IN (SELECT t.id FROM candidates AS t) AND c.revoked_at IS NULL
 ), used AS MATERIALIZED (
  SELECT m.* FROM user_machines AS m WHERE m.id IN (SELECT t.created_by_host_id FROM candidates AS t UNION SELECT c.host_id FROM connectors AS c)
 ), host_rights AS MATERIALIZED (
  SELECT m.id, machine_capability_allowed(actor, m.id, 'tunnel_manage') AS manage FROM used AS m
 ), team_rights AS MATERIALIZED (
  SELECT m.id, t.team_id, workspace_publisher_host_capability_allowed(actor, t.team_id, m.id, 'tunnel_manage') AS manage
  FROM used AS m CROSS JOIN (SELECT c.owner_team_id AS team_id FROM candidates AS c WHERE c.owner_team_id IS NOT NULL
   UNION SELECT workspace_team WHERE workspace_team IS NOT NULL) AS t
 )
 -- Newest first, decided one row at a time until the page is full (OFFSET 0 keeps the order below the filter);
 -- the connector checks run only for the rows that get that far.
 SELECT x.id FROM (
  SELECT t.id, t.account_id, t.owner_team_id, t.access_mode, t.deleted_at IS NOT NULL AS deleted, m.id AS host_id,
   m.user_id AS host_user, m.owner_team_id AS host_team, m.configured_capabilities AS host_capabilities,
   h.manage AS host_manage, own.manage AS own_team_manage, ws.manage AS ws_team_manage
  FROM candidates AS t JOIN used AS m ON m.id = t.created_by_host_id
   LEFT JOIN host_rights AS h ON h.id = m.id
   LEFT JOIN team_rights AS own ON own.id = m.id AND own.team_id = t.owner_team_id
   LEFT JOIN team_rights AS ws ON ws.id = m.id AND ws.team_id = workspace_team
  ORDER BY t.created_at DESC, t.id DESC OFFSET 0
 ) AS x
 WHERE tunnel_management_decision(actor, x.id, x.account_id, x.owner_team_id, x.access_mode, x.deleted, x.host_user,
   x.host_team, x.host_capabilities, x.own_team_manage,
   x.owner_team_id IS NOT NULL AND EXISTS(SELECT 1 FROM connectors AS c LEFT JOIN used AS other ON other.id = c.host_id
    LEFT JOIN team_rights AS r ON r.id = c.host_id AND r.team_id = x.owner_team_id
    WHERE c.tunnel_id = x.id AND tunnel_connector_blocks(actor, x.owner_team_id, x.account_id, x.host_id, x.host_user,
     x.host_team, other.id, other.configured_capabilities, NULL, r.manage)),
   x.host_manage,
   EXISTS(SELECT 1 FROM connectors AS c LEFT JOIN used AS other ON other.id = c.host_id LEFT JOIN host_rights AS r ON r.id = c.host_id
    WHERE c.tunnel_id = x.id AND tunnel_connector_blocks(actor, NULL, x.account_id, x.host_id, x.host_user, x.host_team, other.id,
     other.configured_capabilities, r.manage, NULL)))
  AND (NOT workspace_set OR workspace_tunnel_management_decision(actor, workspace_team, x.id, x.account_id, x.owner_team_id,
   x.access_mode, x.host_user, x.host_team, x.host_capabilities, x.ws_team_manage,
   workspace_team IS NOT NULL AND EXISTS(SELECT 1 FROM connectors AS c LEFT JOIN used AS other ON other.id = c.host_id
    LEFT JOIN team_rights AS r ON r.id = c.host_id AND r.team_id = workspace_team
    WHERE c.tunnel_id = x.id AND tunnel_connector_blocks(actor, workspace_team, x.account_id, x.host_id, x.host_user,
     x.host_team, other.id, other.configured_capabilities, NULL, r.manage))))
 LIMIT row_limit;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.manageable_preview_ids(actor text, workspace_set boolean, workspace_team text, q text, state text, owner text,
    machine text) RETURNS SETOF text
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT manageable_preview_ids(actor, workspace_set, workspace_team, q, state, owner, machine, NULL, NULL, NULL);
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.manageable_tunnel_ids(actor text, workspace_set boolean, workspace_team text, q text, state text, owner text,
    machine text) RETURNS SETOF text
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT manageable_tunnel_ids(actor, workspace_set, workspace_team, q, state, owner, machine, NULL, NULL, NULL);
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.manageable_preview_ids(actor text, workspace_set boolean, workspace_team text, q text, state text, owner text,
    machine text) RETURNS SETOF text
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
 ), used AS MATERIALIZED (
  SELECT m.* FROM user_machines AS m WHERE m.id IN (SELECT c.owner_machine_id FROM candidates AS c)
 ), host_rights AS MATERIALIZED (
  SELECT m.id, machine_capability_allowed(actor, m.id, 'preview_manage') AS manage FROM used AS m
 ), team_rights AS MATERIALIZED (
  SELECT m.id, t.team_id, workspace_publisher_host_capability_allowed(actor, t.team_id, m.id, 'preview_manage') AS manage
  FROM used AS m CROSS JOIN (SELECT c.owner_team_id AS team_id FROM candidates AS c WHERE c.owner_team_id IS NOT NULL
   UNION SELECT workspace_team WHERE workspace_team IS NOT NULL) AS t
 )
 SELECT c.id FROM candidates AS c JOIN used AS m ON m.id = c.owner_machine_id
  LEFT JOIN host_rights AS h ON h.id = m.id
  LEFT JOIN team_rights AS own ON own.id = m.id AND own.team_id = c.owner_team_id
  LEFT JOIN team_rights AS ws ON ws.id = m.id AND ws.team_id = workspace_team
 WHERE preview_management_decision(actor, c.id, c.account_id, c.owner_team_id, c.access_mode, m.user_id, m.owner_team_id,
   m.configured_capabilities, own.manage, h.manage)
  AND (NOT workspace_set OR workspace_preview_management_decision(actor, workspace_team, c.id, c.account_id, c.owner_team_id,
   c.access_mode, m.user_id, m.owner_team_id, m.configured_capabilities, ws.manage));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.manageable_tunnel_ids(actor text, workspace_set boolean, workspace_team text, q text, state text, owner text,
    machine text) RETURNS SETOF text
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 WITH rules AS (
   -- The allow rules for tunnel_manage in the account's teams whose subject may be the account (any role: a superset).
   SELECT tm.team_id, r.policy_id, r.target_kind, r.target_id FROM team_members AS tm
   JOIN workspace_policies AS p ON p.team_id = tm.team_id
   JOIN workspace_policy_rules AS r ON r.policy_id = p.id AND r.action = 'tunnel_manage' AND NOT r.deny
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
  SELECT t.* FROM tunnels AS t
  WHERE t.created_by_host_id IN (SELECT hosts.id FROM hosts)
    AND (t.account_id = actor OR t.access_mode IN ('private', 'team'))
    AND (q='' OR position(lower(q) in lower(t.id||' '||t.name||' '||t.stable_endpoint||' '||t.created_by_host_id))>0)
    AND ((state='' AND t.desired_state<>'deleted') OR t.desired_state=state)
    AND (owner='' OR (owner='mine' AND t.account_id=actor) OR (owner='shared' AND t.account_id<>actor) OR t.account_id=owner)
    AND (machine='' OR t.created_by_host_id=machine OR EXISTS(SELECT 1 FROM tunnel_connectors AS c
      WHERE c.tunnel_id=t.id AND c.host_id=machine AND c.revoked_at IS NULL))
 ), connectors AS MATERIALIZED (
  SELECT c.tunnel_id, c.host_id FROM tunnel_connectors AS c WHERE c.tunnel_id IN (SELECT t.id FROM candidates AS t) AND c.revoked_at IS NULL
 ), used AS MATERIALIZED (
  SELECT m.* FROM user_machines AS m WHERE m.id IN (SELECT t.created_by_host_id FROM candidates AS t UNION SELECT c.host_id FROM connectors AS c)
 ), host_rights AS MATERIALIZED (
  SELECT m.id, machine_capability_allowed(actor, m.id, 'tunnel_manage') AS manage FROM used AS m
 ), team_rights AS MATERIALIZED (
  SELECT m.id, t.team_id, workspace_publisher_host_capability_allowed(actor, t.team_id, m.id, 'tunnel_manage') AS manage
  FROM used AS m CROSS JOIN (SELECT c.owner_team_id AS team_id FROM candidates AS c WHERE c.owner_team_id IS NOT NULL
   UNION SELECT workspace_team WHERE workspace_team IS NOT NULL) AS t
 ), decided AS (
  SELECT t.id AS tunnel_id, m.user_id AS host_user, m.owner_team_id AS host_team, m.configured_capabilities AS host_capabilities,
   h.manage AS host_manage, own.manage AS own_team_manage, ws.manage AS ws_team_manage,
   EXISTS(SELECT 1 FROM connectors AS c LEFT JOIN used AS other ON other.id = c.host_id LEFT JOIN host_rights AS r ON r.id = c.host_id
    WHERE c.tunnel_id = t.id AND tunnel_connector_blocks(actor, NULL, t.account_id, m.id, m.user_id, m.owner_team_id, other.id,
     other.configured_capabilities, r.manage, NULL)) AS personal_blocked,
   EXISTS(SELECT 1 FROM connectors AS c LEFT JOIN used AS other ON other.id = c.host_id
    LEFT JOIN team_rights AS r ON r.id = c.host_id AND r.team_id = t.owner_team_id
    WHERE t.owner_team_id IS NOT NULL AND c.tunnel_id = t.id AND tunnel_connector_blocks(actor, t.owner_team_id, t.account_id, m.id, m.user_id,
     m.owner_team_id, other.id, other.configured_capabilities, NULL, r.manage)) AS own_team_blocked,
   EXISTS(SELECT 1 FROM connectors AS c LEFT JOIN used AS other ON other.id = c.host_id
    LEFT JOIN team_rights AS r ON r.id = c.host_id AND r.team_id = workspace_team
    WHERE workspace_team IS NOT NULL AND c.tunnel_id = t.id AND tunnel_connector_blocks(actor, workspace_team, t.account_id, m.id, m.user_id,
     m.owner_team_id, other.id, other.configured_capabilities, NULL, r.manage)) AS ws_team_blocked
  FROM candidates AS t JOIN used AS m ON m.id = t.created_by_host_id
   LEFT JOIN host_rights AS h ON h.id = m.id
   LEFT JOIN team_rights AS own ON own.id = m.id AND own.team_id = t.owner_team_id
   LEFT JOIN team_rights AS ws ON ws.id = m.id AND ws.team_id = workspace_team
 )
 SELECT t.id FROM candidates AS t JOIN decided AS d ON d.tunnel_id = t.id
 WHERE tunnel_management_decision(actor, t.id, t.account_id, t.owner_team_id, t.access_mode, t.deleted_at IS NOT NULL, d.host_user,
   d.host_team, d.host_capabilities, d.own_team_manage, d.own_team_blocked, d.host_manage, d.personal_blocked)
  AND (NOT workspace_set OR workspace_tunnel_management_decision(actor, workspace_team, t.id, t.account_id, t.owner_team_id, t.access_mode,
   d.host_user, d.host_team, d.host_capabilities, d.ws_team_manage, d.ws_team_blocked));
$$;
-- +goose StatementEnd
DROP FUNCTION paperboat.manageable_tunnel_ids(text, boolean, text, text, text, text, text, timestamptz, text, integer);
DROP FUNCTION paperboat.manageable_preview_ids(text, boolean, text, text, text, text, text, timestamptz, text, integer);
