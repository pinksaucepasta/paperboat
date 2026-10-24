-- Tunnel lifecycle queries are kept in their own sqlc input so tunnel API work
-- can evolve independently from preview leases and route/connector APIs.

-- name: ListPreviewTunnelsV1 :many
-- Every way to manage a tunnel needs its creating host to be the account's own personal machine or a team machine
-- whose workspace policy gives the account tunnel_manage (see ListPreviewLeasesV1): candidates come from
-- that host set by index, and the exact access functions run only until the page is full.
WITH rules AS (
  -- The allow rules for tunnel_manage in the account's teams whose subject may be the account (any role: a superset).
  SELECT tm.team_id, r.policy_id, r.target_kind, r.target_id FROM team_members AS tm
  JOIN workspace_policies AS p ON p.team_id = tm.team_id
  JOIN workspace_policy_rules AS r ON r.policy_id = p.id AND r.action = 'tunnel_manage' AND NOT r.deny
  WHERE tm.account_id = sqlc.arg(account_id) AND tm.active
    AND (r.subject_kind IN ('all', 'role') OR (r.subject_kind = 'account' AND r.subject_id = sqlc.arg(account_id))
      OR (r.subject_kind = 'group' AND EXISTS(SELECT 1 FROM workspace_policy_groups AS g
        WHERE g.policy_id = r.policy_id AND g.group_name = r.subject_id AND g.account_id = sqlc.arg(account_id))))
), targets AS (
  SELECT r.team_id, r.target_id AS machine_id FROM rules AS r WHERE r.target_kind = 'machine'
  UNION
  SELECT r.team_id, tg.machine_id FROM rules AS r
  JOIN workspace_policy_tags AS tg ON tg.policy_id = r.policy_id AND tg.tag = r.target_id WHERE r.target_kind = 'tag'
), hosts AS (
  SELECT m.id FROM user_machines AS m WHERE m.user_id = sqlc.arg(account_id) AND m.owner_team_id IS NULL
  UNION
  SELECT b.resource_id FROM team_resource_bindings AS b JOIN targets AS t ON t.team_id = b.team_id AND t.machine_id = b.resource_id
  WHERE b.resource_kind = 'machine' AND b.active
  UNION
  SELECT b.resource_id FROM team_resource_bindings AS b JOIN rules AS r ON r.team_id = b.team_id AND r.target_kind = 'all_machines'
  WHERE b.resource_kind = 'machine' AND b.active
), candidates AS MATERIALIZED (
  SELECT t.* FROM tunnels AS t
  WHERE t.created_by_host_id IN (SELECT hosts.id FROM hosts)
    -- Every management rule needs the account's own tunnel or a private or team one.
    AND (t.account_id = sqlc.arg(account_id) OR t.access_mode IN ('private', 'team'))
    AND (
      sqlc.narg(after_created_at)::timestamptz IS NULL
      OR (t.created_at, t.id) < (sqlc.narg(after_created_at)::timestamptz, sqlc.narg(after_id)::text)
    )
    AND (sqlc.arg(q)::text='' OR position(lower(sqlc.arg(q)) in lower(t.id||' '||t.name||' '||t.stable_endpoint||' '||t.created_by_host_id))>0)
    AND (sqlc.arg(state)::text='' OR t.desired_state=sqlc.arg(state))
    AND (sqlc.arg(owner)::text='' OR (sqlc.arg(owner)='mine' AND t.account_id=sqlc.arg(account_id)) OR (sqlc.arg(owner)='shared' AND t.account_id<>sqlc.arg(account_id)) OR t.account_id=sqlc.arg(owner))
    -- A machine's tunnels: those it created or hosts an active connector of.
    AND (sqlc.arg(machine_id)::text='' OR t.created_by_host_id=sqlc.arg(machine_id) OR EXISTS(SELECT 1 FROM tunnel_connectors AS c
      WHERE c.tunnel_id=t.id AND c.host_id=sqlc.arg(machine_id) AND c.revoked_at IS NULL))
  ORDER BY t.created_at DESC, t.id DESC
)
SELECT c.id, c.account_id, c.name, c.desired_state, c.access_mode, c.generation, c.stable_endpoint_id, c.stable_endpoint,
       c.created_by_host_id, c.created_by_actor_id, c.expires_at, c.summary_code, c.summary_transitioned_at, c.created_at,
       c.updated_at, c.deleted_at, c.access_generation, c.owner_team_id
FROM candidates AS c
WHERE tunnel_management_allowed(sqlc.arg(account_id)::text, c.id)
 AND (NOT sqlc.arg(workspace_set)::boolean OR workspace_tunnel_management_allowed(sqlc.arg(account_id)::text, sqlc.narg(workspace_team_id)::text, c.id))
LIMIT sqlc.arg(row_limit);

-- name: UpdatePreviewTunnelFieldsV1 :one
UPDATE tunnels
SET name = CASE
      WHEN sqlc.arg(name_set)::boolean THEN sqlc.narg(name)::text
      ELSE name
    END,
    access_mode = CASE
      WHEN sqlc.arg(access_mode_set)::boolean THEN sqlc.narg(access_mode)::text
      ELSE access_mode
    END,
    expires_at = CASE
      WHEN sqlc.arg(expires_at_set)::boolean THEN sqlc.narg(expires_at)::timestamptz
      ELSE expires_at
    END,
    summary_code = CASE
      WHEN sqlc.arg(expires_at_set)::boolean
        AND summary_code = 'expired'
        AND (sqlc.narg(expires_at)::timestamptz IS NULL OR sqlc.narg(expires_at)::timestamptz > sqlc.arg(now)::timestamptz)
        THEN 'pending'
      ELSE summary_code
    END,
    summary_transitioned_at = CASE
      WHEN sqlc.arg(expires_at_set)::boolean
        AND summary_code = 'expired'
        AND (sqlc.narg(expires_at)::timestamptz IS NULL OR sqlc.narg(expires_at)::timestamptz > sqlc.arg(now)::timestamptz)
        THEN sqlc.arg(now)::timestamptz
      ELSE summary_transitioned_at
    END,
    generation = generation + 1,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND account_id = sqlc.arg(account_id)
  AND generation = sqlc.arg(expected_generation)
  AND desired_state <> 'deleted'
RETURNING *;

-- name: GetOwnedActivePreviewTunnelHostV1 :one
SELECT id
FROM user_machines
WHERE id = sqlc.arg(host_id)
  AND machine_capability_allowed(sqlc.arg(account_id)::text,id,'tunnel_manage')
  AND configured_capabilities @> ARRAY['preview_launch']::text[]
  AND deleted_at IS NULL
  AND revoked_at IS NULL
  AND public_identity_key IS NOT NULL
FOR UPDATE;

-- name: ListExpiredPreviewTunnelsV1 :many
SELECT *
FROM tunnels
WHERE expires_at IS NOT NULL
  AND expires_at <= sqlc.arg(now)
  AND desired_state <> 'deleted'
  AND summary_code <> 'expired'
ORDER BY expires_at ASC, id ASC
LIMIT sqlc.arg(row_limit)
FOR UPDATE SKIP LOCKED;

-- name: MarkExpiredPreviewTunnelV1 :one
UPDATE tunnels
SET summary_code = 'expired', summary_transitioned_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND account_id = sqlc.arg(account_id)
  AND expires_at IS NOT NULL
  AND expires_at <= sqlc.arg(now)
  AND desired_state <> 'deleted'
  AND summary_code <> 'expired'
RETURNING *;

-- name: HasActivePrivateTCPRouteV1 :one
SELECT EXISTS (
  SELECT 1
  FROM tunnel_routes
  WHERE tunnel_id = sqlc.arg(tunnel_id)
    AND deleted_at IS NULL
    AND desired_state = 'active'
    AND (protocol = 'private_tcp' OR origin_scheme = 'tcp' OR (protocol = 'http' AND match_type = 'catch_all'))
) AS has_active_private_tcp_route;
