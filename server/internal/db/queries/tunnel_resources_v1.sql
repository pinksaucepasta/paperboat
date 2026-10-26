-- TRK-07 route, domain, connector, enrollment, and safe log queries.

-- name: GetTunnelConfigGenerationV1 :one
SELECT tunnel_id, generation, previous_generation, content_hash, snapshot,
       snapshot_reference, activation_state, created_by_actor_id, created_at,
       activated_at, retained_until
FROM tunnel_config_generations
WHERE tunnel_id = sqlc.arg(tunnel_id)
  AND generation = sqlc.arg(generation);

-- name: GetTunnelForResourceV1 :one
SELECT id, account_id, name, desired_state, access_mode, generation,
       stable_endpoint_id, stable_endpoint, created_by_host_id,
       created_by_actor_id, expires_at, summary_code,
       summary_transitioned_at, created_at, updated_at, deleted_at, access_generation, owner_team_id
FROM tunnels
WHERE id = sqlc.arg(tunnel_id) AND account_id = sqlc.arg(account_id)
FOR UPDATE;

-- name: ListTunnelRoutesV1 :many
SELECT r.id, r.tunnel_id, r.name, r.protocol, r.match_type, r.match_hostname,
       r.wildcard_suffix, r.path_prefix, r.priority, r.origin_scheme, r.origin_address,
       r.preserve_host, r.host_override, r.tls_verification, r.tls_server_name,
       r.ca_reference, r.mtls_credential_reference, r.connect_timeout_ms,
       r.idle_timeout_ms, r.max_concurrent_streams, r.desired_state, r.generation,
       r.created_by_actor_id, r.updated_by_actor_id, r.created_at, r.updated_at,
       r.deleted_at, r.public_tcp_listener_id, r.public_tcp_port
FROM tunnel_routes AS r
JOIN tunnels AS t ON t.id = r.tunnel_id
WHERE r.tunnel_id = sqlc.arg(tunnel_id)
  AND t.account_id = sqlc.arg(account_id)
  AND r.deleted_at IS NULL
  AND (
    sqlc.narg(after_created_at)::timestamptz IS NULL
    OR (r.created_at, r.id) < (
      sqlc.narg(after_created_at)::timestamptz,
      sqlc.narg(after_id)::text
    )
  )
ORDER BY r.created_at DESC, r.id DESC
LIMIT sqlc.arg(row_limit);

-- name: GetTunnelRouteV1 :one
SELECT r.id, r.tunnel_id, r.name, r.protocol, r.match_type, r.match_hostname,
       r.wildcard_suffix, r.path_prefix, r.priority, r.origin_scheme,
       r.origin_address, r.preserve_host, r.host_override, r.tls_verification,
       r.tls_server_name, r.ca_reference, r.mtls_credential_reference,
       r.connect_timeout_ms, r.idle_timeout_ms, r.max_concurrent_streams,
       r.desired_state, r.generation, r.created_by_actor_id,
       r.updated_by_actor_id, r.created_at, r.updated_at, r.deleted_at,
       r.public_tcp_listener_id, r.public_tcp_port
FROM tunnel_routes AS r
JOIN tunnels AS t ON t.id = r.tunnel_id
WHERE r.id = sqlc.arg(route_id)
  AND r.tunnel_id = sqlc.arg(tunnel_id)
  AND t.account_id = sqlc.arg(account_id);

-- name: ListActiveTunnelRoutesForSnapshotV1 :many
SELECT id, tunnel_id, name, protocol, match_type, match_hostname,
       wildcard_suffix, path_prefix, priority, origin_scheme, origin_address,
       preserve_host, host_override, tls_verification, tls_server_name,
       ca_reference, mtls_credential_reference, connect_timeout_ms,
       idle_timeout_ms, max_concurrent_streams, desired_state, generation,
       created_by_actor_id, updated_by_actor_id, created_at, updated_at,
       deleted_at, public_tcp_listener_id, public_tcp_port
FROM tunnel_routes
WHERE tunnel_id = sqlc.arg(tunnel_id)
  AND desired_state = 'active'
  AND deleted_at IS NULL
ORDER BY priority ASC, name ASC, id ASC;

-- name: DeleteAllTunnelRoutesV1 :exec
UPDATE tunnel_routes
SET desired_state = 'deleted', deleted_at = sqlc.arg(now), updated_at = sqlc.arg(now), generation = generation + 1
WHERE tunnel_id = sqlc.arg(tunnel_id) AND desired_state <> 'deleted';

-- name: HasConflictingTunnelRouteV1 :one
SELECT EXISTS (
  SELECT 1
  FROM tunnel_routes
  WHERE tunnel_id = sqlc.arg(tunnel_id)
    AND id <> sqlc.arg(exclude_id)
    AND desired_state = 'active'
    AND deleted_at IS NULL
    AND (protocol = 'tls') = (sqlc.arg(protocol)::text = 'tls')
    AND (
      match_type = sqlc.arg(match_type)
      OR (
        sqlc.arg(match_type) IN ('managed', 'exact')
        AND match_type IN ('managed', 'exact')
      )
    )
    AND path_prefix IS NOT DISTINCT FROM sqlc.narg(path_prefix)
    AND match_hostname IS NOT DISTINCT FROM sqlc.narg(match_hostname)
    AND wildcard_suffix IS NOT DISTINCT FROM sqlc.narg(wildcard_suffix)
) AS has_conflict;

-- name: LockTunnelPublicListenerAllocatorV1 :exec
SELECT pg_advisory_xact_lock(1885686836);

-- name: CreateTunnelRouteV1 :one
WITH public_port AS MATERIALIZED (
  SELECT candidate::integer AS port
  FROM generate_series(sqlc.arg(public_tcp_port_min)::integer, sqlc.arg(public_tcp_port_max)::integer) AS candidate
  WHERE sqlc.arg(protocol)::text = 'tcp'
    AND NOT EXISTS (
      SELECT 1 FROM tunnel_routes
      WHERE public_tcp_port = candidate AND desired_state <> 'deleted'
    )
  ORDER BY candidate
  LIMIT 1
)
INSERT INTO tunnel_routes
  (id, tunnel_id, name, protocol, match_type, match_hostname, wildcard_suffix,
   path_prefix, priority, origin_scheme, origin_address, preserve_host,
   host_override, tls_verification, tls_server_name, ca_reference,
   mtls_credential_reference, connect_timeout_ms, idle_timeout_ms,
   max_concurrent_streams, desired_state, created_by_actor_id,
   public_tcp_listener_id, public_tcp_port,
   updated_by_actor_id, created_at, updated_at)
VALUES
  (sqlc.arg(id), sqlc.arg(tunnel_id), sqlc.arg(name), sqlc.arg(protocol),
   sqlc.arg(match_type), sqlc.narg(match_hostname), sqlc.narg(wildcard_suffix),
   sqlc.narg(path_prefix), sqlc.arg(priority), sqlc.arg(origin_scheme),
   sqlc.arg(origin_address), sqlc.arg(preserve_host), sqlc.narg(host_override),
   sqlc.arg(tls_verification), sqlc.narg(tls_server_name),
   sqlc.narg(ca_reference), sqlc.narg(mtls_credential_reference),
   sqlc.arg(connect_timeout_ms), sqlc.arg(idle_timeout_ms),
   sqlc.arg(max_concurrent_streams), sqlc.arg(desired_state),
   sqlc.arg(created_by_actor_id), CASE WHEN sqlc.arg(protocol)::text='tls' THEN 'listener_tls_443' ELSE sqlc.narg(public_tcp_listener_id) END,
   CASE WHEN sqlc.arg(protocol)::text='tls' THEN 443 ELSE (SELECT port FROM public_port) END, sqlc.arg(updated_by_actor_id),
   sqlc.arg(now), sqlc.arg(now))
RETURNING id, tunnel_id, name, protocol, match_type, match_hostname,
          wildcard_suffix, path_prefix, priority, origin_scheme, origin_address,
          preserve_host, host_override, tls_verification, tls_server_name,
          ca_reference, mtls_credential_reference, connect_timeout_ms,
          idle_timeout_ms, max_concurrent_streams, desired_state, generation,
          created_by_actor_id, updated_by_actor_id, created_at, updated_at,
          deleted_at, public_tcp_listener_id, public_tcp_port;

-- name: UpdateTunnelRouteV1 :one
UPDATE tunnel_routes
SET name = CASE WHEN sqlc.arg(name_set)::boolean THEN sqlc.arg(name)::text ELSE name END,
    protocol = CASE WHEN sqlc.arg(protocol_set)::boolean THEN sqlc.arg(protocol)::text ELSE protocol END,
    match_type = CASE WHEN sqlc.arg(match_type_set)::boolean THEN sqlc.arg(match_type)::text ELSE match_type END,
    match_hostname = CASE WHEN sqlc.arg(match_hostname_set)::boolean THEN sqlc.narg(match_hostname)::text ELSE match_hostname END,
    wildcard_suffix = CASE WHEN sqlc.arg(wildcard_suffix_set)::boolean THEN sqlc.narg(wildcard_suffix)::text ELSE wildcard_suffix END,
    path_prefix = CASE WHEN sqlc.arg(path_prefix_set)::boolean THEN sqlc.narg(path_prefix)::text ELSE path_prefix END,
    priority = CASE WHEN sqlc.arg(priority_set)::boolean THEN sqlc.arg(priority)::integer ELSE priority END,
    origin_scheme = CASE WHEN sqlc.arg(origin_scheme_set)::boolean THEN sqlc.arg(origin_scheme)::text ELSE origin_scheme END,
    origin_address = CASE WHEN sqlc.arg(origin_address_set)::boolean THEN sqlc.arg(origin_address)::text ELSE origin_address END,
    preserve_host = CASE WHEN sqlc.arg(preserve_host_set)::boolean THEN sqlc.arg(preserve_host)::boolean ELSE preserve_host END,
    host_override = CASE WHEN sqlc.arg(host_override_set)::boolean THEN sqlc.narg(host_override)::text ELSE host_override END,
    tls_verification = CASE WHEN sqlc.arg(tls_verification_set)::boolean THEN sqlc.arg(tls_verification)::text ELSE tls_verification END,
    tls_server_name = CASE WHEN sqlc.arg(tls_server_name_set)::boolean THEN sqlc.narg(tls_server_name)::text ELSE tls_server_name END,
    ca_reference = CASE WHEN sqlc.arg(ca_reference_set)::boolean THEN sqlc.narg(ca_reference)::text ELSE ca_reference END,
    mtls_credential_reference = CASE WHEN sqlc.arg(mtls_credential_reference_set)::boolean THEN sqlc.narg(mtls_credential_reference)::text ELSE mtls_credential_reference END,
    connect_timeout_ms = CASE WHEN sqlc.arg(connect_timeout_set)::boolean THEN sqlc.arg(connect_timeout_ms)::integer ELSE connect_timeout_ms END,
    idle_timeout_ms = CASE WHEN sqlc.arg(idle_timeout_set)::boolean THEN sqlc.arg(idle_timeout_ms)::integer ELSE idle_timeout_ms END,
    max_concurrent_streams = CASE WHEN sqlc.arg(max_streams_set)::boolean THEN sqlc.arg(max_concurrent_streams)::integer ELSE max_concurrent_streams END,
    desired_state = CASE WHEN sqlc.arg(desired_state_set)::boolean THEN sqlc.arg(desired_state)::text ELSE desired_state END,
    updated_by_actor_id = sqlc.arg(updated_by_actor_id),
    generation = generation + 1,
    updated_at = sqlc.arg(now),
    deleted_at = CASE WHEN sqlc.arg(desired_state_set)::boolean AND sqlc.arg(desired_state)::text = 'deleted' THEN sqlc.arg(now) ELSE deleted_at END
WHERE id = sqlc.arg(route_id)
  AND tunnel_id = sqlc.arg(tunnel_id)
  AND generation = sqlc.arg(expected_generation)
  AND deleted_at IS NULL
RETURNING id, tunnel_id, name, protocol, match_type, match_hostname,
          wildcard_suffix, path_prefix, priority, origin_scheme, origin_address,
          preserve_host, host_override, tls_verification, tls_server_name,
          ca_reference, mtls_credential_reference, connect_timeout_ms,
          idle_timeout_ms, max_concurrent_streams, desired_state, generation,
          created_by_actor_id, updated_by_actor_id, created_at, updated_at,
          deleted_at, public_tcp_listener_id, public_tcp_port;

-- name: DeleteTunnelRouteV1 :one
UPDATE tunnel_routes
SET desired_state = 'deleted', deleted_at = sqlc.arg(now),
    generation = generation + 1, updated_by_actor_id = sqlc.arg(actor_id),
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(route_id)
  AND tunnel_id = sqlc.arg(tunnel_id)
  AND generation = sqlc.arg(expected_generation)
  AND deleted_at IS NULL
RETURNING id, tunnel_id, name, protocol, match_type, match_hostname,
          wildcard_suffix, path_prefix, priority, origin_scheme, origin_address,
          preserve_host, host_override, tls_verification, tls_server_name,
          ca_reference, mtls_credential_reference, connect_timeout_ms,
          idle_timeout_ms, max_concurrent_streams, desired_state, generation,
          created_by_actor_id, updated_by_actor_id, created_at, updated_at,
          deleted_at, public_tcp_listener_id, public_tcp_port;

-- name: BumpTunnelGenerationForResourceV1 :one
UPDATE tunnels
SET generation = generation + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(tunnel_id)
  AND account_id = sqlc.arg(account_id)
  AND desired_state <> 'deleted'
RETURNING id, account_id, name, desired_state, access_mode, generation,
          stable_endpoint_id, stable_endpoint, created_by_host_id,
          created_by_actor_id, expires_at, summary_code,
          summary_transitioned_at, created_at, updated_at, deleted_at, access_generation, owner_team_id;

-- name: ListTunnelDomainsV1 :many
SELECT tunnel_domains.*
FROM tunnel_domains
WHERE account_id = sqlc.arg(account_id)
  AND tunnel_id = sqlc.arg(tunnel_id)
  AND deleted_at IS NULL
  AND (
    sqlc.narg(after_created_at)::timestamptz IS NULL
    OR (created_at, id) < (
      sqlc.narg(after_created_at)::timestamptz,
      sqlc.narg(after_id)::text
    )
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit);

-- name: GetTunnelDomainV1 :one
SELECT d.*
FROM tunnel_domains AS d
WHERE d.id = sqlc.arg(domain_id)
  AND d.account_id = sqlc.arg(account_id)
  AND d.tunnel_id = sqlc.arg(tunnel_id);

-- name: GetTunnelDomainByHostnameV1 :one
SELECT tunnel_domains.*
FROM tunnel_domains
WHERE hostname = sqlc.arg(hostname)
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: CreateTunnelDomainV1 :one
INSERT INTO tunnel_domains
  (id, account_id, tunnel_id, route_id, hostname, match_type,
   ownership_challenge_reference, ownership_state, dns_target,
   observed_records, certificate_strategy, certificate_state, caa_state,
   conflict_state, generation, created_at, updated_at,
   dns_provider, expected_records, dns_next_check_at)
VALUES
  (sqlc.arg(id), sqlc.arg(account_id), sqlc.arg(tunnel_id), sqlc.arg(route_id),
   sqlc.arg(hostname), sqlc.arg(match_type),
   sqlc.arg(ownership_challenge_reference), 'pending', sqlc.arg(dns_target),
   sqlc.arg(observed_records)::jsonb, sqlc.arg(certificate_strategy),
   CASE WHEN sqlc.arg(certificate_strategy) = 'none' THEN 'not_applicable' ELSE 'pending' END,
   CASE WHEN sqlc.arg(certificate_strategy) = 'none' THEN 'not_applicable' ELSE 'unknown' END, 'clear',
   1, sqlc.arg(now), sqlc.arg(now), sqlc.arg(dns_provider),
   sqlc.arg(expected_records)::jsonb, sqlc.arg(now))
RETURNING *;

-- name: BeginTunnelDomainVerificationV1 :one
UPDATE tunnel_domains
SET ownership_state = CASE WHEN ownership_state IN ('pending','failed') THEN 'pending' ELSE ownership_state END,
    conflict_state = CASE WHEN conflict_state = 'conflicted' THEN 'clear' ELSE conflict_state END,
    dns_next_check_at = sqlc.arg(now), verification_attempts = 0,
    generation = generation + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(domain_id)
  AND account_id = sqlc.arg(account_id)
  AND tunnel_id = sqlc.arg(tunnel_id)
  AND generation = sqlc.arg(expected_generation)
  AND deleted_at IS NULL
RETURNING *;

-- name: DeleteTunnelDomainV1 :one
UPDATE tunnel_domains
SET ownership_state = 'revoked', conflict_state = 'quarantined',
    quarantine_until = sqlc.arg(quarantine_until), generation = generation + 1,
    deleted_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(domain_id)
  AND account_id = sqlc.arg(account_id)
  AND tunnel_id = sqlc.arg(tunnel_id)
  AND generation = sqlc.arg(expected_generation)
  AND deleted_at IS NULL
RETURNING *;

-- name: MarkTunnelDomainVerifiedV1 :one
UPDATE tunnel_domains
SET ownership_state = 'verified', last_verified_at = sqlc.arg(now),
    generation = generation + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(domain_id)
  AND account_id = sqlc.arg(account_id)
  AND tunnel_id = sqlc.arg(tunnel_id)
  AND deleted_at IS NULL
RETURNING *;

-- name: CreateTunnelLogEntryV1 :one
INSERT INTO tunnel_log_entries
  (id, account_id, tunnel_id, preview_id, route_id, connector_id, session_id, level,
   component, code, message, metadata, correlation_id, occurred_at)
VALUES
  (sqlc.arg(id), sqlc.arg(account_id), sqlc.arg(tunnel_id),
   sqlc.narg(preview_id), sqlc.narg(route_id), sqlc.narg(connector_id), sqlc.narg(session_id),
   sqlc.arg(level), sqlc.arg(component), sqlc.arg(code), sqlc.arg(message),
   sqlc.arg(metadata)::jsonb, sqlc.arg(correlation_id), sqlc.arg(occurred_at))
RETURNING id, account_id, tunnel_id, preview_id, route_id, connector_id, session_id,
          level, component, code, message, metadata, correlation_id,
          occurred_at, cursor_sequence;

-- name: ListTunnelLogsV1 :many
SELECT id, account_id, tunnel_id, route_id, connector_id, session_id, level,
       preview_id,
       component, code, message, metadata, correlation_id, occurred_at,
       cursor_sequence
FROM tunnel_log_entries
WHERE account_id = sqlc.arg(account_id)
  AND tunnel_id = sqlc.arg(tunnel_id)
  AND cursor_sequence > sqlc.arg(after_sequence)
ORDER BY cursor_sequence ASC
LIMIT sqlc.arg(row_limit);

-- name: ListPreviewLogsV1 :many
SELECT id, account_id, tunnel_id, preview_id, route_id, connector_id, session_id,
       level, component, code, message, metadata, correlation_id, occurred_at,
       cursor_sequence
FROM tunnel_log_entries
WHERE account_id = sqlc.arg(account_id)
  AND preview_id = sqlc.arg(preview_id)
  AND cursor_sequence > sqlc.arg(after_sequence)
ORDER BY cursor_sequence ASC
LIMIT sqlc.arg(row_limit);

-- name: ListTunnelDomainsByIDsV1 :many
-- The rows of a page of the workspace domain list (domaininventory), whose query authorized them.
SELECT * FROM tunnel_domains WHERE id = ANY(sqlc.arg(ids)::text[]) AND deleted_at IS NULL;
