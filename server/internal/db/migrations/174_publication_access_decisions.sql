-- +goose Up
-- The preview and tunnel lists could not count what they list: their access functions run the host checks
-- (machine_capability_allowed, workspace_publisher_host_capability_allowed) for every row, about 2.5 ms each, so
-- the lists stopped at the page and a total meant 33 s at 20k previews. The rules now live in decision functions
-- that take the host checks as arguments: the access functions call them with the checks for one publication (as
-- before, a check only when its branch needs it), and manageable_preview_ids / manageable_tunnel_ids, which the lists
-- and their counts read, call them with each host's checks computed once per host. The rules themselves are
-- unchanged (TestPublicationAccessAnswersAreUnchangedByMigration174OnPostgres).

-- workspace_preview_management_allowed's rule; host_manage is
-- workspace_publisher_host_capability_allowed(actor, team, host, 'preview_manage') (unused when team is NULL).
-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_preview_management_decision(actor text, team text, preview_id text, preview_account text,
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

-- preview_management_allowed's rule; team_host_manage is the check above for the preview's own team, host_manage
-- machine_capability_allowed(actor, host, 'preview_manage').
-- +goose StatementBegin
CREATE FUNCTION paperboat.preview_management_decision(actor text, preview_id text, preview_account text, preview_team text,
    access_mode text, host_user text, host_team text, host_capabilities text[], team_host_manage boolean, host_manage boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT (preview_team IS NOT NULL AND workspace_preview_management_decision(actor, preview_team, preview_id, preview_account, preview_team,
   access_mode, host_user, host_team, host_capabilities, team_host_manage))
  OR (preview_team IS NULL AND ((preview_account=actor AND host_team IS NULL AND host_user=actor)
   OR (access_mode IN ('private','team') AND host_capabilities @> ARRAY['preview_launch']::text[] AND coalesce(host_manage, false))));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.workspace_preview_management_allowed(actor text, team text, preview text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM preview_leases p JOIN user_machines m ON m.id=p.owner_machine_id WHERE p.id=preview
  AND workspace_preview_management_decision(actor, team, p.id, p.account_id, p.owner_team_id, p.access_mode, m.user_id, m.owner_team_id,
   m.configured_capabilities, CASE WHEN team IS NOT NULL THEN workspace_publisher_host_capability_allowed(actor, team, m.id, 'preview_manage') END));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.preview_management_allowed(actor text, preview text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM preview_leases p JOIN user_machines m ON m.id=p.owner_machine_id WHERE p.id=preview
  AND preview_management_decision(actor, p.id, p.account_id, p.owner_team_id, p.access_mode, m.user_id, m.owner_team_id, m.configured_capabilities,
   CASE WHEN p.owner_team_id IS NOT NULL THEN workspace_publisher_host_capability_allowed(actor, p.owner_team_id, m.id, 'preview_manage') END,
   CASE WHEN p.owner_team_id IS NULL AND p.access_mode IN ('private','team') THEN machine_capability_allowed(actor, m.id, 'preview_manage') END));
$$;
-- +goose StatementEnd

-- A tunnel connector stops personal management (tunnel_management_allowed) when its host is gone, the actor may
-- not manage tunnels on it (host_manage: machine_capability_allowed) or it cannot publish; in a team workspace
-- (team_host_manage: workspace_publisher_host_capability_allowed) a personal tunnel's own host need not publish.
-- +goose StatementBegin
CREATE FUNCTION paperboat.tunnel_connector_blocks(actor text, team text, tunnel_account text, creating_host text, creating_host_user text,
    creating_host_team text, connector_host text, connector_host_capabilities text[], host_manage boolean, team_host_manage boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT CASE WHEN team IS NULL THEN connector_host IS NULL OR NOT coalesce(host_manage, false)
   OR NOT connector_host_capabilities @> ARRAY['preview_launch']::text[]
  ELSE connector_host IS NULL OR NOT coalesce(team_host_manage, false)
   OR (NOT connector_host_capabilities @> ARRAY['preview_launch']::text[]
    AND NOT (connector_host=creating_host AND tunnel_account=actor AND creating_host_user=actor AND creating_host_team IS NULL)) END;
$$;
-- +goose StatementEnd

-- workspace_tunnel_management_allowed's rule; connectors_blocked: an active connector blocks (tunnel_connector_blocks
-- in the workspace's team; unused when team is NULL).
-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_tunnel_management_decision(actor text, team text, tunnel_id text, tunnel_account text, tunnel_team text,
    access_mode text, host_user text, host_team text, host_capabilities text[], host_manage boolean, connectors_blocked boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT (CASE WHEN team IS NULL THEN tunnel_team IS NULL AND tunnel_account=actor AND host_team IS NULL AND host_user=actor
  ELSE (host_capabilities @> ARRAY['preview_launch']::text[] OR (tunnel_account=actor AND host_user=actor AND host_team IS NULL))
   AND (access_mode IN ('private','team') OR (tunnel_account=actor AND host_user=actor AND host_team IS NULL))
   AND (tunnel_team=team OR (tunnel_team IS NULL AND EXISTS(SELECT 1 FROM team_resource_bindings b WHERE b.team_id=team
    AND b.resource_kind='tunnel' AND b.resource_id=tunnel_id AND b.owner_account=tunnel_account AND b.active))) END)
  AND (team IS NULL OR coalesce(host_manage, false))
  AND (team IS NULL OR NOT coalesce(connectors_blocked, true));
$$;
-- +goose StatementEnd

-- tunnel_management_allowed's rule: team_* are the workspace checks for the tunnel's own team, host_manage and
-- connectors_blocked the personal ones.
-- +goose StatementBegin
CREATE FUNCTION paperboat.tunnel_management_decision(actor text, tunnel_id text, tunnel_account text, tunnel_team text, access_mode text,
    deleted boolean, host_user text, host_team text, host_capabilities text[], team_host_manage boolean, team_connectors_blocked boolean,
    host_manage boolean, connectors_blocked boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT (tunnel_team IS NOT NULL AND workspace_tunnel_management_decision(actor, tunnel_team, tunnel_id, tunnel_account, tunnel_team,
   access_mode, host_user, host_team, host_capabilities, team_host_manage, team_connectors_blocked))
  OR (tunnel_team IS NULL AND ((tunnel_account=actor AND host_team IS NULL AND host_user=actor)
   OR (access_mode IN ('private','team') AND NOT deleted AND host_capabilities @> ARRAY['preview_launch']::text[]
    AND coalesce(host_manage, false) AND NOT coalesce(connectors_blocked, true))));
$$;
-- +goose StatementEnd

-- The connectors of one tunnel that block, with each host check computed here (one tunnel at a time).
-- +goose StatementBegin
CREATE FUNCTION paperboat.tunnel_connectors_blocked(actor text, team text, tunnel text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM tunnels t JOIN user_machines m ON m.id=t.created_by_host_id
  JOIN tunnel_connectors c ON c.tunnel_id=t.id AND c.revoked_at IS NULL LEFT JOIN user_machines other ON other.id=c.host_id
  WHERE t.id=tunnel AND tunnel_connector_blocks(actor, team, t.account_id, m.id, m.user_id, m.owner_team_id, other.id, other.configured_capabilities,
   CASE WHEN team IS NULL AND other.id IS NOT NULL THEN machine_capability_allowed(actor, other.id, 'tunnel_manage') END,
   CASE WHEN team IS NOT NULL AND other.id IS NOT NULL THEN workspace_publisher_host_capability_allowed(actor, team, other.id, 'tunnel_manage') END));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.workspace_tunnel_management_allowed(actor text, team text, tunnel text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM tunnels t JOIN user_machines m ON m.id=t.created_by_host_id WHERE t.id=tunnel
  AND workspace_tunnel_management_decision(actor, team, t.id, t.account_id, t.owner_team_id, t.access_mode, m.user_id, m.owner_team_id,
   m.configured_capabilities, CASE WHEN team IS NOT NULL THEN workspace_publisher_host_capability_allowed(actor, team, m.id, 'tunnel_manage') END,
   CASE WHEN team IS NOT NULL THEN tunnel_connectors_blocked(actor, team, t.id) END));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.tunnel_management_allowed(actor text, tunnel text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM tunnels t JOIN user_machines m ON m.id=t.created_by_host_id WHERE t.id=tunnel
  AND tunnel_management_decision(actor, t.id, t.account_id, t.owner_team_id, t.access_mode, t.deleted_at IS NOT NULL, m.user_id, m.owner_team_id,
   m.configured_capabilities,
   CASE WHEN t.owner_team_id IS NOT NULL THEN workspace_publisher_host_capability_allowed(actor, t.owner_team_id, m.id, 'tunnel_manage') END,
   CASE WHEN t.owner_team_id IS NOT NULL THEN tunnel_connectors_blocked(actor, t.owner_team_id, t.id) END,
   CASE WHEN t.owner_team_id IS NULL AND t.access_mode IN ('private','team') THEN machine_capability_allowed(actor, m.id, 'tunnel_manage') END,
   CASE WHEN t.owner_team_id IS NULL AND t.access_mode IN ('private','team') THEN tunnel_connectors_blocked(actor, NULL, t.id) END));
$$;
-- +goose StatementEnd

-- The ids of the previews actor may manage, filtered like the list (workspace, q, state, owner, machine): the list query's
-- candidates (hosts the account may manage previews on), each host check computed once per host and per host and
-- team, and the same decisions as the access functions. The list pages it and the count counts it.
-- +goose StatementBegin
CREATE FUNCTION paperboat.manageable_preview_ids(actor text, workspace_set boolean, workspace_team text, q text, state text, owner text,
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
    AND (state='' OR CASE WHEN p.terminal_state='owner_lost' THEN 'owner_disconnected' WHEN p.terminal_state='expired' THEN 'expired' WHEN p.terminal_state IN ('stopped','failed') THEN 'stopped' WHEN p.allocation_state<>'ready' THEN 'allocating' WHEN p.edge_state<>'ready' OR p.origin_state<>'ready' THEN 'connecting' ELSE 'ready' END=state OR (state='active' AND p.terminal_state='active'))
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

-- The ids of the tunnels actor may manage, like manageable_preview_ids; connector hosts get their checks once per host too.
-- +goose StatementBegin
CREATE FUNCTION paperboat.manageable_tunnel_ids(actor text, workspace_set boolean, workspace_team text, q text, state text, owner text,
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
    AND (state='' OR t.desired_state=state)
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

-- +goose Down
DROP FUNCTION paperboat.manageable_tunnel_ids(text, boolean, text, text, text, text, text);
DROP FUNCTION paperboat.manageable_preview_ids(text, boolean, text, text, text, text, text);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.workspace_preview_management_allowed(actor text, team text, preview text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM preview_leases p JOIN user_machines m ON m.id=p.owner_machine_id
 WHERE p.id=preview
 AND (CASE WHEN team IS NULL THEN p.owner_team_id IS NULL AND p.account_id=actor AND m.owner_team_id IS NULL AND m.user_id=actor
 ELSE (m.configured_capabilities @> ARRAY['preview_launch']::text[] OR (p.account_id=actor AND m.user_id=actor AND m.owner_team_id IS NULL)) AND (p.access_mode IN ('private','team') OR (p.account_id=actor AND m.user_id=actor AND m.owner_team_id IS NULL)) AND (p.owner_team_id=team OR (p.owner_team_id IS NULL AND EXISTS(
  SELECT 1 FROM team_resource_bindings b WHERE b.team_id=team AND b.resource_kind='preview' AND b.resource_id=p.id AND b.owner_account=p.account_id AND b.active
 ))) END) AND (team IS NULL OR workspace_publisher_host_capability_allowed(actor,team,m.id,'preview_manage')));
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.workspace_tunnel_management_allowed(actor text, team text, tunnel text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM tunnels t JOIN user_machines m ON m.id=t.created_by_host_id
 WHERE t.id=tunnel
 AND (CASE WHEN team IS NULL THEN t.owner_team_id IS NULL AND t.account_id=actor AND m.owner_team_id IS NULL AND m.user_id=actor
 ELSE (m.configured_capabilities @> ARRAY['preview_launch']::text[] OR (t.account_id=actor AND m.user_id=actor AND m.owner_team_id IS NULL)) AND (t.access_mode IN ('private','team') OR (t.account_id=actor AND m.user_id=actor AND m.owner_team_id IS NULL)) AND (t.owner_team_id=team OR (t.owner_team_id IS NULL AND EXISTS(
  SELECT 1 FROM team_resource_bindings b WHERE b.team_id=team AND b.resource_kind='tunnel' AND b.resource_id=t.id AND b.owner_account=t.account_id AND b.active
 ))) END) AND (team IS NULL OR workspace_publisher_host_capability_allowed(actor,team,m.id,'tunnel_manage'))
 AND (team IS NULL OR NOT EXISTS(SELECT 1 FROM tunnel_connectors c LEFT JOIN user_machines other ON other.id=c.host_id
 WHERE c.tunnel_id=t.id AND c.revoked_at IS NULL AND (other.id IS NULL OR NOT workspace_publisher_host_capability_allowed(actor,team,other.id,'tunnel_manage') OR (NOT other.configured_capabilities @> ARRAY['preview_launch']::text[] AND NOT (other.id=m.id AND t.account_id=actor AND m.user_id=actor AND m.owner_team_id IS NULL))))));
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.preview_management_allowed(actor text, preview text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM preview_leases p JOIN user_machines m ON m.id=p.owner_machine_id WHERE p.id=preview AND (
 (p.owner_team_id IS NOT NULL AND workspace_preview_management_allowed(actor,p.owner_team_id,p.id))
 OR (p.owner_team_id IS NULL AND ((p.account_id=actor AND m.owner_team_id IS NULL AND m.user_id=actor)
 OR (p.access_mode IN ('private','team') AND m.configured_capabilities @> ARRAY['preview_launch']::text[] AND machine_capability_allowed(actor,m.id,'preview_manage'))))));
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.tunnel_management_allowed(actor text, tunnel text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM tunnels t JOIN user_machines m ON m.id=t.created_by_host_id WHERE t.id=tunnel AND (
 (t.owner_team_id IS NOT NULL AND workspace_tunnel_management_allowed(actor,t.owner_team_id,t.id))
 OR (t.owner_team_id IS NULL AND ((t.account_id=actor AND m.owner_team_id IS NULL AND m.user_id=actor)
 OR (t.access_mode IN ('private','team') AND t.deleted_at IS NULL AND m.configured_capabilities @> ARRAY['preview_launch']::text[]
 AND machine_capability_allowed(actor,m.id,'tunnel_manage') AND NOT EXISTS(SELECT 1 FROM tunnel_connectors c LEFT JOIN user_machines other ON other.id=c.host_id
 WHERE c.tunnel_id=t.id AND c.revoked_at IS NULL AND (other.id IS NULL OR NOT machine_capability_allowed(actor,other.id,'tunnel_manage') OR NOT other.configured_capabilities @> ARRAY['preview_launch']::text[])))))));
$$;
-- +goose StatementEnd
DROP FUNCTION paperboat.tunnel_connectors_blocked(text, text, text);
DROP FUNCTION paperboat.tunnel_management_decision(text, text, text, text, text, boolean, text, text, text[], boolean, boolean, boolean, boolean);
DROP FUNCTION paperboat.workspace_tunnel_management_decision(text, text, text, text, text, text, text, text, text[], boolean, boolean);
DROP FUNCTION paperboat.tunnel_connector_blocks(text, text, text, text, text, text, text, text[], boolean, boolean);
DROP FUNCTION paperboat.preview_management_decision(text, text, text, text, text, text, text, text[], boolean, boolean);
DROP FUNCTION paperboat.workspace_preview_management_decision(text, text, text, text, text, text, text, text, text[], boolean);
