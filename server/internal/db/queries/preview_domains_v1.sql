-- name: ListPreviewDomainsV1 :many
SELECT *
FROM preview_domains
WHERE account_id = sqlc.arg(account_id)
  AND preview_id = sqlc.arg(preview_id)
  AND deleted_at IS NULL
  AND (
    sqlc.narg(after_created_at)::timestamptz IS NULL
    OR (created_at, id) > (
      sqlc.narg(after_created_at)::timestamptz,
      sqlc.narg(after_id)::text
    )
  )
ORDER BY created_at, id
LIMIT sqlc.arg(row_limit);

-- name: ListPreviewDomainProjectionsV1 :many
-- The domain projections of a page of previews in one query (the caller keeps each preview's own account's rows).
SELECT *
FROM preview_domains
WHERE preview_id = ANY(sqlc.arg(preview_ids)::text[])
ORDER BY preview_id, created_at, id;

-- name: GetPreviewDomainV1 :one
SELECT *
FROM preview_domains
WHERE id = sqlc.arg(domain_id)
  AND account_id = sqlc.arg(account_id)
  AND preview_id = sqlc.arg(preview_id)
  AND deleted_at IS NULL;

-- name: GetPreviewLeaseForDomainV1 :one
SELECT id, account_id, generation, lease_deadline, user_deadline,
       allocation_state, edge_state, origin_state, terminal_state
FROM preview_leases
WHERE id = sqlc.arg(preview_id)
  AND account_id = sqlc.arg(account_id)
FOR UPDATE;

-- name: GetPreviewLeaseOwnerContextV1 :one
SELECT id, account_id, generation, lease_deadline, user_deadline,
       allocation_state, edge_state, origin_state, terminal_state,
       owner_machine_id, owner_session_id, endpoint
FROM preview_leases
WHERE id = sqlc.arg(preview_id)
  AND account_id = sqlc.arg(account_id)
FOR UPDATE;

-- name: GetPreviewDomainLeaseViewV1 :one
SELECT id, account_id, generation, lease_deadline, user_deadline,
       allocation_state, edge_state, origin_state, terminal_state,
       owner_machine_id, owner_session_id, endpoint
FROM preview_leases
WHERE id = sqlc.arg(preview_id)
  AND account_id = sqlc.arg(account_id);

-- name: GetPreviewDomainAnyV1 :one
SELECT *
FROM preview_domains
WHERE id = sqlc.arg(domain_id)
  AND account_id = sqlc.arg(account_id)
  AND preview_id = sqlc.arg(preview_id);

-- name: CreatePreviewDomainV1 :one
INSERT INTO preview_domains (
  id, account_id, preview_id, preview_generation, hostname, match_type,
  ownership_challenge_reference, ownership_state, dns_target,
  observed_records, dns_provider, expected_records, dns_next_check_at,
  certificate_strategy, certificate_state, caa_state, conflict_state,
  generation, created_at, updated_at, workspace_domain_id, last_verified_at
) VALUES (
  sqlc.arg(id), sqlc.arg(account_id), sqlc.arg(preview_id), sqlc.arg(preview_generation),
  sqlc.arg(hostname), sqlc.arg(match_type), sqlc.arg(ownership_challenge_reference),
  'verified', sqlc.arg(dns_target), '[]'::jsonb, sqlc.arg(dns_provider),
  sqlc.arg(expected_records), sqlc.arg(now), sqlc.arg(certificate_strategy),
  CASE WHEN sqlc.arg(certificate_strategy) = 'none' THEN 'not_applicable' ELSE 'issuing' END, -- verified at creation: the certificate is next
  CASE WHEN sqlc.arg(certificate_strategy) = 'none' THEN 'not_applicable' ELSE 'unknown' END,
  'clear', 1, sqlc.arg(now), sqlc.arg(now), sqlc.arg(workspace_domain_id), sqlc.arg(now)
)
RETURNING *;

-- name: ApplyPreviewDomainCertificateObservationV1 :one
UPDATE preview_domains
SET certificate_state = sqlc.arg(certificate_state),
    certificate_reference = sqlc.narg(certificate_reference),
    certificate_expires_at = sqlc.narg(certificate_expires_at),
    certificate_renewal_attempted_at = sqlc.arg(now),
    certificate_failure_code = sqlc.narg(failure_code),
    caa_state = sqlc.arg(caa_state),
    generation = generation + 1,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(domain_id)
  AND account_id = sqlc.arg(account_id)
  AND preview_id = sqlc.arg(preview_id)
  AND generation = sqlc.arg(expected_generation)
  AND ownership_state = 'verified'
  AND deleted_at IS NULL
RETURNING *;

-- name: DeletePreviewDomainV1 :one
UPDATE preview_domains
SET ownership_state = 'revoked',
    certificate_state = CASE WHEN certificate_state = 'not_applicable' THEN 'not_applicable' ELSE 'revoked' END,
    conflict_state = 'quarantined',
    quarantine_until = sqlc.arg(quarantine_until),
    deleted_at = sqlc.arg(now),
    generation = generation + 1,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(domain_id)
  AND account_id = sqlc.arg(account_id)
  AND preview_id = sqlc.arg(preview_id)
  AND generation = sqlc.arg(expected_generation)
  AND deleted_at IS NULL
RETURNING *;

-- name: AdvancePreviewDomainTargetGenerationV1 :execrows
UPDATE preview_domains
SET preview_generation = sqlc.arg(new_preview_generation),
    updated_at = sqlc.arg(now)
WHERE preview_id = sqlc.arg(preview_id)
  AND account_id = sqlc.arg(account_id)
  AND preview_generation = sqlc.arg(previous_preview_generation)
  AND deleted_at IS NULL;

-- name: AdvancePreviewDomainsWithLeaseGenerationV1 :execrows
WITH live_lease AS (
  SELECT preview_leases.id, preview_leases.account_id, preview_leases.generation
  FROM preview_leases
  WHERE preview_leases.id = sqlc.arg(preview_id)
    AND preview_leases.account_id = sqlc.arg(account_id)
    AND preview_leases.generation = sqlc.arg(previous_preview_generation)
    AND preview_leases.terminal_state = 'active'
  FOR UPDATE
)
UPDATE preview_domains AS d
SET preview_generation = live_lease.generation + 1,
    updated_at = sqlc.arg(now)
FROM live_lease
WHERE d.preview_id = live_lease.id
  AND d.account_id = live_lease.account_id
  AND d.preview_generation = sqlc.arg(previous_preview_generation)
  AND d.deleted_at IS NULL;

-- name: RenewPreviewLeaseAndAdvanceDomainsV1 :one
WITH renewed AS (
  UPDATE preview_leases
  SET lease_deadline = sqlc.arg(lease_deadline),
      last_renewed_at = sqlc.arg(now),
      owner_last_seen_at = sqlc.arg(now),
      generation = generation + 1
  WHERE preview_leases.id = sqlc.arg(preview_id)
    AND preview_leases.account_id = sqlc.arg(account_id)
    AND preview_leases.owner_machine_id = sqlc.arg(owner_machine_id)
    AND preview_leases.owner_session_id = sqlc.arg(owner_session_id)
    AND preview_leases.terminal_state = 'active'
    AND preview_leases.generation = sqlc.arg(expected_generation)
    AND (preview_leases.user_deadline IS NULL OR sqlc.arg(lease_deadline) <= preview_leases.user_deadline)
  RETURNING id, account_id, generation
), moved AS (
  UPDATE preview_domains AS d
  SET preview_generation = renewed.generation,
      updated_at = sqlc.arg(now)
  FROM renewed
  WHERE d.preview_id = renewed.id
    AND d.account_id = renewed.account_id
    AND d.preview_generation = sqlc.arg(expected_generation)
    AND d.deleted_at IS NULL
  RETURNING d.id
)
SELECT renewed.id, renewed.account_id, renewed.generation
FROM renewed;

-- name: StopPreviewLeaseAndWithdrawDomainsV1 :one
WITH stopped AS (
  UPDATE preview_leases
  SET allocation_state = 'released',
      edge_state = 'released',
      terminal_state = sqlc.arg(terminal_state),
      generation = generation + 1,
      stopped_at = sqlc.arg(now)
  WHERE preview_leases.id = sqlc.arg(preview_id)
    AND preview_leases.account_id = sqlc.arg(account_id)
    AND preview_leases.terminal_state = 'active'
    AND preview_leases.generation = sqlc.arg(expected_generation)
  RETURNING id, account_id, generation
), withdrawn AS (
  UPDATE preview_domains AS d
  SET ownership_state = CASE WHEN sqlc.arg(terminal_state) = 'owner_lost' THEN 'expired' ELSE 'revoked' END,
      certificate_state = CASE WHEN d.certificate_state = 'not_applicable' THEN 'not_applicable' ELSE 'revoked' END,
      conflict_state = 'quarantined',
      quarantine_until = sqlc.arg(quarantine_until),
      deleted_at = sqlc.arg(now),
      generation = d.generation + 1,
      updated_at = sqlc.arg(now)
  FROM stopped
  WHERE d.preview_id = stopped.id
    AND d.account_id = stopped.account_id
    AND d.preview_generation <= sqlc.arg(expected_generation)
    AND d.deleted_at IS NULL
  RETURNING d.id
)
SELECT stopped.id, stopped.account_id, stopped.generation
FROM stopped;

-- name: ExpirePreviewLeaseAndWithdrawDomainsV1 :many
WITH candidates AS (
  SELECT source.id
  FROM preview_leases AS source
  WHERE source.terminal_state = 'active'
    AND LEAST(source.lease_deadline, COALESCE(source.user_deadline, source.lease_deadline)) <= sqlc.arg(now)
  ORDER BY LEAST(source.lease_deadline, COALESCE(source.user_deadline, source.lease_deadline)), source.id
  LIMIT sqlc.arg(row_limit)
  FOR UPDATE SKIP LOCKED
), expired AS (
  UPDATE preview_leases AS p
  SET allocation_state = 'released', edge_state = 'released', terminal_state = 'expired',
      generation = p.generation + 1, stopped_at = sqlc.arg(now)
  FROM candidates
  WHERE p.id = candidates.id
  RETURNING p.id, p.account_id, p.generation
), withdrawn AS (
  UPDATE preview_domains AS d
  SET ownership_state = 'expired',
      certificate_state = CASE WHEN d.certificate_state = 'not_applicable' THEN 'not_applicable' ELSE 'revoked' END,
      conflict_state = 'quarantined', quarantine_until = sqlc.arg(quarantine_until),
      deleted_at = sqlc.arg(now), generation = d.generation + 1, updated_at = sqlc.arg(now)
  FROM expired
  WHERE d.preview_id = expired.id AND d.account_id = expired.account_id
    AND d.preview_generation < expired.generation AND d.deleted_at IS NULL
  RETURNING d.id
)
SELECT id, account_id, generation FROM expired;

-- name: MarkLostPreviewLeaseAndWithdrawDomainsV1 :many
WITH candidates AS (
  SELECT source.id
  FROM preview_leases AS source
  WHERE source.terminal_state = 'active'
    AND source.lease_deadline > sqlc.arg(now)
    AND source.owner_last_seen_at <= sqlc.arg(owner_cutoff)
  ORDER BY source.owner_last_seen_at, source.id
  LIMIT sqlc.arg(row_limit)
  FOR UPDATE SKIP LOCKED
), lost AS (
  UPDATE preview_leases AS p
  SET allocation_state = 'released', edge_state = 'released', terminal_state = 'owner_lost',
      generation = p.generation + 1, stopped_at = sqlc.arg(now)
  FROM candidates
  WHERE p.id = candidates.id
  RETURNING p.id, p.account_id, p.generation
), withdrawn AS (
  UPDATE preview_domains AS d
  SET ownership_state = 'expired',
      certificate_state = CASE WHEN d.certificate_state = 'not_applicable' THEN 'not_applicable' ELSE 'revoked' END,
      conflict_state = 'quarantined', quarantine_until = sqlc.arg(quarantine_until),
      deleted_at = sqlc.arg(now), generation = d.generation + 1, updated_at = sqlc.arg(now)
  FROM lost
  WHERE d.preview_id = lost.id AND d.account_id = lost.account_id
    AND d.preview_generation < lost.generation AND d.deleted_at IS NULL
  RETURNING d.id
)
SELECT id, account_id, generation FROM lost;

-- name: WithdrawPreviewDomainsV1 :many
UPDATE preview_domains
SET ownership_state = 'expired',
    certificate_state = CASE WHEN certificate_state = 'not_applicable' THEN 'not_applicable' ELSE 'revoked' END,
    conflict_state = 'quarantined',
    quarantine_until = sqlc.arg(quarantine_until),
    deleted_at = sqlc.arg(now),
    generation = generation + 1,
    updated_at = sqlc.arg(now)
WHERE preview_id = sqlc.arg(preview_id)
  AND account_id = sqlc.arg(account_id)
  AND preview_generation <= sqlc.arg(terminal_preview_generation)
  AND deleted_at IS NULL
RETURNING *;

-- name: ListReadyPreviewDomainsV1 :many
SELECT d.*
FROM preview_domains AS d
JOIN preview_leases AS p
  ON p.id = d.preview_id AND p.account_id = d.account_id
WHERE d.account_id = sqlc.arg(account_id)
  AND d.preview_id = sqlc.arg(preview_id)
  AND d.deleted_at IS NULL
  AND d.ownership_state = 'verified'
  AND d.conflict_state = 'clear'
  AND d.certificate_state = 'ready'
  AND d.certificate_reference IS NOT NULL
  AND p.terminal_state = 'active'
  AND p.generation = d.preview_generation
  AND p.lease_deadline > sqlc.arg(now)
  AND (p.user_deadline IS NULL OR p.user_deadline > sqlc.arg(now))
ORDER BY d.hostname, d.id;

-- name: ListReadyPreviewDomainAliasesV1 :many
SELECT d.*, cert.certificate_reference AS active_certificate_reference,
       cert.certificate_generation AS active_certificate_generation
FROM preview_domains AS d
JOIN preview_leases AS p
  ON p.id = d.preview_id AND p.account_id = d.account_id
JOIN tunnel_certificate_records AS cert
  ON cert.target_kind = 'preview_lease'
 AND cert.account_id = d.account_id
 AND cert.preview_id = d.preview_id
 AND cert.preview_generation = d.preview_generation
 AND cert.hostname = d.hostname
 AND cert.state = 'active'
 AND cert.preview_state = 'active'
 AND cert.certificate_reference = d.certificate_reference
 AND cert.expires_at > sqlc.arg(now)
WHERE d.account_id = sqlc.arg(account_id)
  AND d.preview_id = sqlc.arg(preview_id)
  AND d.deleted_at IS NULL
  AND d.ownership_state = 'verified'
  AND d.conflict_state = 'clear'
  AND d.certificate_state = 'ready'
  AND d.certificate_reference IS NOT NULL
  AND p.terminal_state = 'active'
  AND p.generation = d.preview_generation
  AND p.lease_deadline > sqlc.arg(now)
  AND (p.user_deadline IS NULL OR p.user_deadline > sqlc.arg(now))
ORDER BY d.hostname, d.id;

-- name: GetPreviewDomainForReconciliationV1 :one
SELECT d.*
FROM preview_domains AS d
JOIN preview_leases AS p
  ON p.id = d.preview_id AND p.account_id = d.account_id
WHERE d.id = sqlc.arg(domain_id)
  AND d.deleted_at IS NULL
  AND p.terminal_state = 'active'
  AND p.generation = d.preview_generation
FOR UPDATE OF d;
