-- Tunnel lifecycle queries are kept in their own sqlc input so tunnel API work
-- can evolve independently from preview leases and route/connector APIs.

-- name: ListPreviewTunnelsV1 :many
-- One page of the tunnels the account may manage (manageable_tunnel_ids, migration 174).
SELECT c.id, c.account_id, c.name, c.desired_state, c.access_mode, c.generation, c.stable_endpoint_id, c.stable_endpoint,
       c.created_by_host_id, c.created_by_actor_id, c.expires_at, c.summary_code, c.summary_transitioned_at, c.created_at,
       c.updated_at, c.deleted_at, c.access_generation, c.owner_team_id
FROM tunnels AS c
WHERE c.id IN (SELECT paperboat.manageable_tunnel_ids(sqlc.arg(account_id)::text, sqlc.arg(workspace_set)::boolean, sqlc.narg(workspace_team_id)::text,
  sqlc.arg(q)::text, sqlc.arg(state)::text, sqlc.arg(owner)::text, sqlc.arg(machine_id)::text,
  sqlc.narg(after_created_at)::timestamptz, sqlc.narg(after_id)::text, sqlc.arg(row_limit)::integer))
ORDER BY c.created_at DESC, c.id DESC
LIMIT sqlc.arg(row_limit);

-- name: CountPreviewTunnelsV1 :one
-- Every tunnel ListPreviewTunnelsV1 can page through with the same filters.
SELECT count(*) FROM tunnels AS c WHERE c.id IN (SELECT paperboat.manageable_tunnel_ids(sqlc.arg(account_id)::text, sqlc.arg(workspace_set)::boolean,
  sqlc.narg(workspace_team_id)::text, sqlc.arg(q)::text, sqlc.arg(state)::text, sqlc.arg(owner)::text, sqlc.arg(machine_id)::text, NULL, NULL, NULL));

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
