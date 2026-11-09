-- name: ApplyTunnelDomainCertificateObservationV1 :one
UPDATE tunnel_domains
SET certificate_state = sqlc.arg(certificate_state),
    certificate_reference = sqlc.narg(certificate_reference),
    certificate_expires_at = sqlc.narg(certificate_expires_at),
    certificate_renewal_attempted_at = sqlc.arg(now),
    certificate_failure_code = sqlc.narg(failure_code),
    caa_state = sqlc.arg(caa_state),
    generation = generation + 1,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(domain_id)
  AND generation = sqlc.arg(expected_generation)
  AND ownership_state = 'verified'
  AND deleted_at IS NULL
RETURNING *;
