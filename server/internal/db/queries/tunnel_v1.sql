-- Tunnel lifecycle queries are kept in their own sqlc input so tunnel API work
-- can evolve independently from preview leases and route/connector APIs.

-- name: ListPreviewTunnelsV1 :many
-- Every way to manage a tunnel needs its creating host to be the account's own personal machine or one with a
-- tunnel_manage grant for the account in one of its active teams (see ListPreviewLeasesV1): candidates come from
-- that host set by index, and the exact access functions run only until the page is full.
WITH hosts AS (
  SELECT m.id FROM user_machines AS m WHERE m.user_id = sqlc.arg(account_id) AND m.owner_team_id IS NULL
  UNION
  SELECT g.machine_id FROM team_machine_grants AS g
  JOIN team_members AS tm ON tm.team_id = g.team_id AND tm.account_id = sqlc.arg(account_id) AND tm.active
  WHERE g.active AND 'tunnel_manage' = ANY(g.capabilities) AND (g.audience = 'all_members' OR g.account_id = sqlc.arg(account_id))
), candidates AS MATERIALIZED (
  SELECT t.* FROM tunnels AS t
  WHERE t.created_by_host_id IN (SELECT hosts.id FROM hosts)
    AND (
      sqlc.narg(after_created_at)::timestamptz IS NULL
      OR (t.created_at, t.id) < (sqlc.narg(after_created_at)::timestamptz, sqlc.narg(after_id)::text)
    )
    AND (sqlc.arg(q)::text='' OR position(lower(sqlc.arg(q)) in lower(t.id||' '||t.name||' '||t.stable_endpoint||' '||t.created_by_host_id))>0)
    AND (sqlc.arg(state)::text='' OR t.desired_state=sqlc.arg(state))
    AND (sqlc.arg(owner)::text='' OR (sqlc.arg(owner)='mine' AND t.account_id=sqlc.arg(account_id)) OR (sqlc.arg(owner)='shared' AND t.account_id<>sqlc.arg(account_id)) OR t.account_id=sqlc.arg(owner))
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
