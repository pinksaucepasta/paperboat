-- Preview leases are foreground/session-owned resources. Keep their persistence
-- separate from durable tunnel queries so lease cleanup can be reconciled without
-- changing tunnel identity, routes, or domains.

-- name: VerifyPreviewLeaseOwnerV1 :one
SELECT owned.id
FROM user_machines AS owned
WHERE owned.id = sqlc.arg(owner_machine_id)
  AND owned.user_id = sqlc.arg(account_id)
  AND owned.state = 'online'
  AND owned.online
  AND owned.deleted_at IS NULL
  AND owned.revoked_at IS NULL
LIMIT 1;

-- name: GetPreviewLeaseV1 :one
SELECT p.id, p.endpoint, p.account_id, p.actor_id, p.owner_machine_id, p.owner_session_id,
       p.target_scheme, p.target_address, p.access_mode, p.lease_deadline, p.user_deadline,
       p.allocation_state, p.edge_state, p.origin_state, p.terminal_state, p.created_at, p.ready_at,
       p.last_renewed_at, p.stopped_at, p.generation, p.owner_last_seen_at,p.owner_session_kind,p.owner_team_id
FROM preview_leases AS p
WHERE p.id = sqlc.arg(id) AND p.account_id = sqlc.arg(account_id);

-- name: GetPreviewLeaseForReconciliationV1 :one
SELECT p.id, p.endpoint, p.account_id, p.actor_id, p.owner_machine_id, p.owner_session_id,
       p.target_scheme, p.target_address, p.access_mode, p.lease_deadline, p.user_deadline,
       p.allocation_state, p.edge_state, p.origin_state, p.terminal_state, p.created_at, p.ready_at,
       p.last_renewed_at, p.stopped_at, p.generation, p.owner_last_seen_at,p.owner_session_kind,p.owner_team_id
FROM preview_leases AS p
WHERE p.id = sqlc.arg(id);

-- name: GetPreviewLeaseCreateOperationV1 :one
SELECT operation.id, operation.account_id, operation.idempotency_key, operation.request_hash, operation.operation_type, operation.resource_kind, operation.resource_id,
       operation.phase, operation.state, operation.progress, operation.retrying, operation.next_retry_at, operation.error_code, operation.outcome, operation.result_reference,
       operation.correlation_id, operation.created_at, operation.updated_at, operation.completed_at
FROM operations AS operation
JOIN preview_lease_create_operations AS create_operation
  ON create_operation.account_id = operation.account_id
 AND create_operation.preview_id = operation.resource_id
 AND create_operation.operation_id = operation.id
WHERE operation.account_id = sqlc.arg(account_id)
  AND operation.resource_kind = 'preview_lease'
  AND operation.resource_id = sqlc.arg(resource_id)
  AND operation.operation_type = 'preview.create';

-- name: CompletePreviewLeaseCreateOperationV1 :one
UPDATE operations
SET phase = 'ready', state = 'succeeded', progress = 100,
    retrying = false, outcome = 'changed', updated_at = sqlc.arg(updated_at),
    completed_at = sqlc.arg(completed_at)
WHERE id = sqlc.arg(id) AND account_id = sqlc.arg(account_id)
  AND state IN ('pending','running')
RETURNING id, account_id, idempotency_key, request_hash, operation_type, resource_kind, resource_id,
          phase, state, progress, retrying, next_retry_at, error_code, outcome, result_reference,
          correlation_id, created_at, updated_at, completed_at;

-- name: FailPreviewLeaseCreateOperationV1 :one
UPDATE operations
SET phase = 'failed', state = 'failed', progress = 100,
    retrying = false, error_code = sqlc.arg(error_code), outcome = 'unchanged',
    updated_at = sqlc.arg(updated_at), completed_at = sqlc.arg(completed_at)
WHERE id = sqlc.arg(id) AND account_id = sqlc.arg(account_id)
  AND state IN ('pending','running')
RETURNING id, account_id, idempotency_key, request_hash, operation_type, resource_kind, resource_id,
          phase, state, progress, retrying, next_retry_at, error_code, outcome, result_reference,
          correlation_id, created_at, updated_at, completed_at;

-- A dispatch timeout or connection loss is not proof that the host did not
-- accept the one-shot request. Keep the lease active and mark only the
-- original create operation uncertain so a later replay can retry the same
-- operation/lease without reporting readiness.
-- name: MarkPreviewLeaseCreateOperationUncertainV1 :one
UPDATE operations AS operation
SET phase = 'dispatch', state = 'uncertain', progress = 60,
    retrying = true, error_code = sqlc.arg(error_code), outcome = 'uncertain',
    updated_at = sqlc.arg(updated_at), completed_at = NULL
WHERE operation.id = (
	SELECT create_operation.operation_id
	FROM preview_lease_create_operations AS create_operation
	JOIN operations AS candidate
	  ON candidate.id = create_operation.operation_id
	 AND candidate.account_id = create_operation.account_id
	WHERE candidate.account_id = sqlc.arg(account_id)
	  AND create_operation.preview_id = sqlc.arg(resource_id)
	  AND candidate.resource_kind = 'preview_lease'
	  AND candidate.resource_id = sqlc.arg(resource_id)
	  AND candidate.operation_type = 'preview.create'
)
AND operation.account_id = sqlc.arg(account_id)
AND operation.state IN ('pending','running','uncertain')
RETURNING id, account_id, idempotency_key, request_hash, operation_type, resource_kind, resource_id,
          phase, state, progress, retrying, next_retry_at, error_code, outcome, result_reference,
          correlation_id, created_at, updated_at, completed_at;

-- name: ListPreviewLeasesV1 :many
-- Every way to manage a preview needs its host machine to be the account's own personal machine or a machine shared
-- into one of its active teams whose workspace policy gives the account preview_manage there. Ordering those candidates first and running the
-- exact access functions only until the page is full keeps the list independent of how many previews exist: it
-- scanned every lease with ~2.5 ms of function calls per row (33 s at 20k leases).
WITH rules AS (
  -- The allow rules for preview_manage in the account's teams whose subject may be the account (any role: a superset).
  SELECT tm.team_id, r.policy_id, r.target_kind, r.target_id FROM team_members AS tm
  JOIN workspace_policies AS p ON p.team_id = tm.team_id
  JOIN workspace_policy_rules AS r ON r.policy_id = p.id AND r.action = 'preview_manage' AND NOT r.deny
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
  SELECT p.* FROM preview_leases AS p
  WHERE p.owner_machine_id IN (SELECT hosts.id FROM hosts)
    -- Every management rule needs the account's own preview or a private or team one.
    AND (p.account_id = sqlc.arg(account_id) OR p.access_mode IN ('private', 'team'))
    AND (
      sqlc.narg(after_created_at)::timestamptz IS NULL
      OR (p.created_at, p.id) < (sqlc.narg(after_created_at)::timestamptz, sqlc.narg(after_id)::text)
    )
    AND (sqlc.arg(q)::text='' OR position(lower(sqlc.arg(q)) in lower(p.id||' '||p.endpoint||' '||p.owner_machine_id))>0)
    AND (sqlc.arg(state)::text='' OR CASE WHEN p.terminal_state='owner_lost' THEN 'owner_disconnected' WHEN p.terminal_state='expired' THEN 'expired' WHEN p.terminal_state IN ('stopped','failed') THEN 'stopped' WHEN p.allocation_state<>'ready' THEN 'allocating' WHEN p.edge_state<>'ready' OR p.origin_state<>'ready' THEN 'connecting' ELSE 'ready' END=sqlc.arg(state) OR (sqlc.arg(state)='active' AND p.terminal_state='active'))
    AND (sqlc.arg(owner)::text='' OR (sqlc.arg(owner)='mine' AND p.account_id=sqlc.arg(account_id)) OR (sqlc.arg(owner)='shared' AND p.account_id<>sqlc.arg(account_id)) OR p.account_id=sqlc.arg(owner))
    AND (sqlc.arg(machine_id)::text='' OR p.owner_machine_id=sqlc.arg(machine_id))
  ORDER BY p.created_at DESC, p.id DESC
)
SELECT c.id, c.endpoint, c.account_id, c.actor_id, c.owner_machine_id, c.owner_session_id,
       c.target_scheme, c.target_address, c.access_mode, c.lease_deadline, c.user_deadline,
       c.allocation_state, c.edge_state, c.origin_state, c.terminal_state, c.created_at, c.ready_at,
       c.last_renewed_at, c.stopped_at, c.generation, c.owner_last_seen_at,c.owner_session_kind,c.owner_team_id
FROM candidates AS c
WHERE preview_management_allowed(sqlc.arg(account_id), c.id)
 AND (NOT sqlc.arg(workspace_set)::boolean OR workspace_preview_management_allowed(sqlc.arg(account_id)::text, sqlc.narg(workspace_team_id)::text, c.id))
LIMIT sqlc.arg(row_limit);

-- name: AdvancePreviewLeaseLifecycleV1 :one
UPDATE preview_leases
SET generation = generation + 1,
    owner_last_seen_at = sqlc.arg(owner_last_seen_at)
WHERE id = sqlc.arg(preview_id)
  AND account_id = sqlc.arg(account_id)
  AND owner_machine_id = sqlc.arg(owner_machine_id)
  AND owner_session_id = sqlc.arg(owner_session_id)
  AND terminal_state = 'active'
  AND generation = sqlc.arg(expected_generation)
RETURNING id AS lifecycle_preview_id,
          generation AS lifecycle_generation,
          owner_last_seen_at AS lifecycle_owner_last_seen_at,
          created_at AS lifecycle_created_at;

-- Readiness observations are authorized by the connector/edge worker rather
-- than by the foreground owner session. They still advance the same strong
-- generation, but deliberately use a separate CAS so a worker cannot borrow
-- owner credentials to mutate a lease.
-- name: AdvancePreviewLeaseReadinessV1 :execrows
UPDATE preview_leases
SET generation = generation + 1
WHERE id = sqlc.arg(preview_id)
  AND account_id = sqlc.arg(account_id)
  AND terminal_state = 'active'
  AND generation = sqlc.arg(expected_generation);

-- name: RenewPreviewLeaseV1 :exec
UPDATE preview_leases
SET lease_deadline = sqlc.arg(lease_deadline),
    last_renewed_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND account_id = sqlc.arg(account_id)
  AND terminal_state = 'active'
  AND (user_deadline IS NULL OR sqlc.arg(lease_deadline) <= user_deadline);

-- name: StopPreviewLeaseV1 :exec
UPDATE preview_leases
SET allocation_state = 'released',
    edge_state = 'released',
    terminal_state = CASE
      WHEN LEAST(lease_deadline, COALESCE(user_deadline, lease_deadline)) <= sqlc.arg(now)
      THEN 'expired'
      ELSE 'stopped'
    END,
    stopped_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND account_id = sqlc.arg(account_id)
  AND terminal_state = 'active';

-- name: MarkPreviewLeaseReadyV1 :exec
UPDATE preview_leases
SET allocation_state = sqlc.arg(allocation_state),
    edge_state = sqlc.arg(edge_state),
    origin_state = sqlc.arg(origin_state),
    terminal_state = CASE
      WHEN sqlc.arg(allocation_state) = 'failed' THEN 'failed'
      ELSE terminal_state
    END,
    stopped_at = CASE
      WHEN sqlc.arg(allocation_state) = 'failed' THEN COALESCE(stopped_at, sqlc.arg(now))
      ELSE stopped_at
    END,
    ready_at = CASE
      WHEN sqlc.arg(allocation_state) = 'ready'
       AND sqlc.arg(edge_state) = 'ready'
       AND sqlc.arg(origin_state) = 'ready'
      THEN COALESCE(ready_at, sqlc.arg(now))
      ELSE ready_at
    END
WHERE id = sqlc.arg(id)
  AND account_id = sqlc.arg(account_id)
  AND terminal_state = 'active';

-- name: ExpirePreviewLeasesV1 :many
WITH candidates AS (
  SELECT source.id
  FROM preview_leases AS source
  WHERE source.terminal_state = 'active'
    AND LEAST(source.lease_deadline, COALESCE(source.user_deadline, source.lease_deadline)) <= sqlc.arg(now)
  ORDER BY LEAST(source.lease_deadline, COALESCE(source.user_deadline, source.lease_deadline)), source.id
  LIMIT sqlc.arg(row_limit)
)
UPDATE preview_leases AS p
SET allocation_state = 'released',
    edge_state = 'released',
    terminal_state = 'expired',
    generation = generation + 1,
    stopped_at = sqlc.arg(now)
FROM candidates
WHERE p.id = candidates.id
RETURNING p.id;

-- name: MarkLostPreviewLeasesV1 :many
WITH candidates AS (
  SELECT source.id
  FROM preview_leases AS source
  WHERE source.terminal_state = 'active'
    AND source.lease_deadline > sqlc.arg(now)
    AND ((source.owner_last_seen_at <= sqlc.arg(owner_cutoff)
        AND NOT (source.owner_session_kind='local_lease' AND source.user_deadline IS NOT NULL))
      -- A background preview is owned by the host runtime, which may be restarting (an update);
      -- it keeps its URL for a longer grace so the runtime can resume it.
      OR (source.owner_last_seen_at <= sqlc.arg(background_owner_cutoff)
        AND source.owner_session_kind='local_lease' AND source.user_deadline IS NOT NULL)
      OR (source.owner_session_kind='lazy_runtime' AND NOT EXISTS (
        SELECT 1 FROM lazy_access_policies policy
        JOIN lazy_runtime_owners runtime ON runtime.machine_id=policy.machine_id
        JOIN user_machines machine ON machine.id=policy.machine_id
        JOIN lazy_activations activation ON activation.policy_id=policy.id
        WHERE 'https://'||policy.hostname=source.endpoint
          AND policy.account_id=source.account_id AND policy.machine_id=source.owner_machine_id
          AND policy.target_scheme=source.target_scheme AND policy.target_address=source.target_address AND policy.access_mode=source.access_mode
          AND activation.state IN ('running','ready')
          AND (activation.preview_id IS NULL OR activation.preview_id=source.id)
          AND policy.deleted_at IS NULL AND policy.expires_at>sqlc.arg(now)
          AND policy.generation=activation.policy_generation
          AND runtime.boot_id=activation.boot_id AND runtime.expires_at>sqlc.arg(now)
          AND source.owner_session_id=activation.owner_session_id
          AND runtime.account_id=policy.account_id
          AND runtime.installation_generation=policy.installation_generation
          AND machine.installation_generation=policy.installation_generation
          AND machine.environment_id=policy.environment_id AND machine.seat_state = 'occupied' AND machine.state NOT IN ('revoked','deleted')
          AND machine.user_id=policy.account_id AND machine.deleted_at IS NULL AND machine.revoked_at IS NULL
      )))
  ORDER BY source.owner_last_seen_at, source.id
  LIMIT sqlc.arg(row_limit)
)
UPDATE preview_leases AS p
SET allocation_state = 'released',
    edge_state = 'released',
    terminal_state = 'owner_lost',
    generation = generation + 1,
    stopped_at = sqlc.arg(now)
FROM candidates
WHERE p.id = candidates.id
RETURNING p.id;
