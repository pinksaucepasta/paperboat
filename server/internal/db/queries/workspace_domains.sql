-- name: CreateWorkspaceDomain :one
INSERT INTO workspace_domains (id, account_id, owner_team_id, hostname, challenge_reference, created_by_actor_id, created_at, updated_at, dns_next_check_at)
VALUES (sqlc.arg(id), sqlc.arg(account_id), sqlc.narg(owner_team_id), sqlc.arg(hostname), sqlc.arg(challenge_reference),
        sqlc.arg(created_by_actor_id), sqlc.arg(now), sqlc.arg(now), sqlc.arg(now))
ON CONFLICT (hostname) DO NOTHING
RETURNING *;

-- name: GetWorkspaceDomainByHostname :one
SELECT * FROM workspace_domains WHERE hostname = sqlc.arg(hostname);

-- A workspace is a personal workspace (team NULL, owned by the account) or a team.
-- name: GetWorkspaceDomain :one
SELECT * FROM workspace_domains
WHERE id = sqlc.arg(id)
  AND ((sqlc.narg(team_id)::text IS NULL AND owner_team_id IS NULL AND account_id = sqlc.arg(account_id))
    OR (sqlc.narg(team_id)::text IS NOT NULL AND owner_team_id = sqlc.narg(team_id)::text));

-- name: ListWorkspaceDomains :many
SELECT * FROM workspace_domains
WHERE (sqlc.narg(team_id)::text IS NULL AND owner_team_id IS NULL AND account_id = sqlc.arg(account_id))
   OR (sqlc.narg(team_id)::text IS NOT NULL AND owner_team_id = sqlc.narg(team_id)::text)
ORDER BY hostname
LIMIT 500;

-- The workspace domain a tunnel or preview hostname lives under: the hostname itself, or its parent
-- (one label below the domain, which the domain's wildcard certificate covers). The longest match wins.
-- name: GetCoveringWorkspaceDomain :one
SELECT * FROM workspace_domains
WHERE hostname IN (sqlc.arg(hostname)::text, sqlc.arg(parent)::text)
  AND ((sqlc.narg(team_id)::text IS NULL AND owner_team_id IS NULL AND account_id = sqlc.arg(account_id))
    OR (sqlc.narg(team_id)::text IS NOT NULL AND owner_team_id = sqlc.narg(team_id)::text))
ORDER BY length(hostname) DESC
LIMIT 1;

-- name: DeleteWorkspaceDomain :execrows
DELETE FROM workspace_domains WHERE id = sqlc.arg(id);

-- name: CheckWorkspaceDomainNow :one
UPDATE workspace_domains
SET dns_next_check_at = sqlc.arg(now), verification_attempts = 0,
    certificate_retry_at = CASE WHEN certificate_state = 'failed' THEN NULL ELSE certificate_retry_at END,
    certificate_attempts = CASE WHEN certificate_state = 'failed' THEN 0 ELSE certificate_attempts END,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: ListDueWorkspaceDomainDNS :many
SELECT * FROM workspace_domains
WHERE dns_next_check_at <= sqlc.arg(now)
ORDER BY dns_next_check_at, id
LIMIT sqlc.arg(row_limit);

-- name: ApplyWorkspaceDomainDNSObservation :exec
UPDATE workspace_domains
SET ownership_state = sqlc.arg(ownership_state),
    dns_failure_code = sqlc.narg(dns_failure_code),
    observed_records = sqlc.arg(observed_records)::jsonb,
    dns_last_checked_at = sqlc.arg(now),
    dns_next_check_at = sqlc.arg(next_check_at),
    verification_attempts = CASE WHEN sqlc.arg(ownership_state) = 'verified' THEN 0 ELSE verification_attempts + 1 END,
    verified_at = CASE WHEN sqlc.arg(ownership_state) = 'verified' THEN coalesce(verified_at, sqlc.arg(now)) ELSE verified_at END,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: ListDueWorkspaceDomainCertificates :many
SELECT * FROM workspace_domains
WHERE ownership_state = 'verified'
  AND (certificate_retry_at IS NULL OR certificate_retry_at <= sqlc.arg(now))
  AND (certificate_expires_at IS NULL OR certificate_expires_at <= sqlc.arg(renew_after))
ORDER BY certificate_expires_at NULLS FIRST, id
LIMIT sqlc.arg(row_limit);

-- name: StoreWorkspaceDomainCertificate :execrows
UPDATE workspace_domains
SET certificate_state = 'ready', certificate_failure_code = NULL, certificate_attempts = 0, certificate_retry_at = NULL,
    master_key_reference = sqlc.arg(master_key_reference), certificate_ciphertext = sqlc.arg(certificate_ciphertext),
    private_key_ciphertext = sqlc.arg(private_key_ciphertext), certificate_fingerprint = sqlc.arg(certificate_fingerprint),
    certificate_expires_at = sqlc.arg(certificate_expires_at), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND ownership_state = 'verified';

-- A failure keeps the last good certificate: only a domain without one shows failed.
-- name: MarkWorkspaceDomainCertificateFailure :exec
UPDATE workspace_domains
SET certificate_state = CASE WHEN certificate_ciphertext IS NULL THEN 'failed' ELSE certificate_state END,
    certificate_failure_code = sqlc.arg(failure_code), certificate_attempts = certificate_attempts + 1,
    certificate_retry_at = sqlc.arg(retry_at), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- The tunnels and previews that use a workspace domain's hostnames.
-- name: ListWorkspaceDomainUses :many
SELECT 'tunnel'::text AS kind, d.workspace_domain_id::text AS workspace_domain_id, d.id AS domain_id, d.hostname,
       t.id AS resource_id, t.name AS resource_name, d.certificate_state
FROM tunnel_domains d JOIN tunnels t ON t.id = d.tunnel_id
WHERE d.workspace_domain_id = ANY(sqlc.arg(ids)::text[]) AND d.deleted_at IS NULL AND t.desired_state <> 'deleted'
UNION ALL
SELECT 'preview', d.workspace_domain_id::text, d.id, d.hostname, p.id, p.endpoint, d.certificate_state
FROM preview_domains d JOIN preview_leases p ON p.id = d.preview_id
WHERE d.workspace_domain_id = ANY(sqlc.arg(ids)::text[]) AND d.deleted_at IS NULL AND p.terminal_state = 'active'
ORDER BY 2, 4, 3;
