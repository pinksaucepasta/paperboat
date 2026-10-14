-- name: ListDueTunnelCertificateDomainsV1 :many
SELECT d.*
FROM tunnel_domains AS d
WHERE d.deleted_at IS NULL
  AND d.ownership_state = 'verified'
  AND d.conflict_state = 'clear'
  AND (
    d.certificate_state IN ('issuing','renewing','failed')
    OR d.certificate_expires_at <= sqlc.arg(now)::timestamptz + sqlc.arg(renew_before)::interval
    OR (
      d.certificate_state = 'ready'
      AND EXISTS (
        SELECT 1
        FROM tunnel_certificate_records AS cert
        JOIN tunnel_edge_route_assignments AS a
          ON a.route_id = d.route_id
         AND a.tunnel_id = d.tunnel_id
         AND a.account_id = d.account_id
        WHERE cert.domain_id = d.id
          AND cert.certificate_reference = d.certificate_reference
          AND cert.target_kind = 'durable_route'
          AND cert.leaf_hostname IS NULL
          AND cert.state = 'active'
          AND a.state = 'active'
          AND a.observed_state = 'ready'
          AND NOT EXISTS (
            SELECT 1
            FROM tunnel_certificate_edge_distributions AS edge_cert
            WHERE edge_cert.certificate_id = cert.id
              AND edge_cert.edge_node_id = a.edge_node_id
              AND edge_cert.edge_process_epoch = a.edge_process_epoch
              AND edge_cert.edge_assignment_generation = a.assignment_generation
              AND edge_cert.observed_certificate_generation = cert.certificate_generation
              AND edge_cert.state = 'active'
          )
      )
    )
  )
ORDER BY d.certificate_expires_at NULLS FIRST, d.id
LIMIT sqlc.arg(row_limit);

-- name: ListReadyTunnelCertificateEdgesV1 :many
SELECT a.edge_node_id, a.edge_process_epoch, a.assignment_generation
FROM tunnel_edge_route_assignments AS a
JOIN tunnel_domains AS d ON d.route_id = a.route_id AND d.tunnel_id = a.tunnel_id AND d.account_id = a.account_id
WHERE d.id = sqlc.arg(domain_id)
  AND a.state = 'active'
  AND a.observed_state = 'ready'
ORDER BY a.edge_node_id, a.edge_process_epoch, a.assignment_generation;

-- name: ListOnDemandWildcardDomainsForEdgeV1 :many
SELECT d.*
FROM tunnel_domains AS d
JOIN tunnel_edge_route_assignments AS a
  ON a.route_id = d.route_id
 AND a.tunnel_id = d.tunnel_id
 AND a.account_id = d.account_id
WHERE d.certificate_strategy = 'on_demand_leaf'
  AND d.match_type = 'one_label_wildcard'
  AND d.ownership_state = 'verified'
  AND d.conflict_state = 'clear'
  AND d.certificate_state = 'ready'
  AND d.certificate_reference IS NOT NULL
  AND d.deleted_at IS NULL
  AND a.edge_node_id = sqlc.arg(edge_node_id)
  AND a.edge_process_epoch = sqlc.arg(edge_process_epoch)
  AND a.state = 'active'
  AND a.observed_state = 'ready'
  AND EXISTS (
    SELECT 1
    FROM tunnel_certificate_records AS cert
    WHERE cert.domain_id = d.id
      AND cert.certificate_reference = d.certificate_reference
      AND cert.target_kind = 'durable_route'
      AND cert.leaf_hostname IS NULL
      AND cert.state = 'active'
  )
ORDER BY d.id
LIMIT sqlc.arg(row_limit);

-- Preview domains are lease-bound aliases, so their certificate worker uses
-- the same DNS/certificate/distribution due rules as durable domains while
-- retaining the preview target fence. Lease identity and expiry are resolved
-- separately by the worker before issuance.
-- name: ListDuePreviewCertificateDomainsV1 :many
SELECT d.*
FROM preview_domains AS d
JOIN preview_leases AS lease
  ON lease.id = d.preview_id AND lease.account_id = d.account_id
WHERE d.deleted_at IS NULL
  AND d.ownership_state = 'verified'
  AND d.conflict_state = 'clear'
  AND (d.certificate_failure_code IS NULL OR d.certificate_renewal_attempted_at IS NULL OR d.certificate_renewal_attempted_at <= sqlc.arg(now)::timestamptz - interval '1 minute')
  AND lease.terminal_state = 'active'
  AND (lease.owner_session_kind<>'lazy_runtime' OR EXISTS (SELECT 1 FROM lazy_access_policies policy WHERE policy.account_id=lease.account_id AND policy.machine_id=lease.owner_machine_id AND lease.endpoint='https://'||policy.hostname AND policy.target_scheme=lease.target_scheme AND policy.target_address=lease.target_address AND policy.access_mode=lease.access_mode AND policy.deleted_at IS NULL))
  AND lease.generation = d.preview_generation
  AND lease.lease_deadline > sqlc.arg(now)
  AND (lease.user_deadline IS NULL OR lease.user_deadline > sqlc.arg(now))
  AND (
    d.certificate_state IN ('pending','issuing','renewing','failed')
    OR d.certificate_expires_at <= sqlc.arg(now)::timestamptz + sqlc.arg(renew_before)::interval
    OR (
      d.certificate_state = 'ready'
      AND EXISTS (
        SELECT 1
        FROM tunnel_certificate_records AS cert
        WHERE cert.domain_id = d.id
          AND cert.account_id = d.account_id
          AND cert.target_kind = 'preview_lease'
          AND cert.preview_id = d.preview_id
          AND cert.preview_generation = d.preview_generation
          AND cert.certificate_reference = d.certificate_reference
          AND cert.leaf_hostname IS NULL
          AND cert.state = 'active'
          AND cert.expires_at > sqlc.arg(now)
          AND EXISTS (
            SELECT 1
            FROM preview_lease_carrier_attachments AS attachment
            WHERE attachment.account_id = d.account_id
              AND attachment.preview_id = d.preview_id
              AND attachment.lease_generation > 0 AND attachment.lease_generation <= d.preview_generation
              AND attachment.owner_session_id=lease.owner_session_id AND attachment.host_id=lease.owner_machine_id
              AND attachment.state = 'ready'
              AND attachment.edge_ready
              AND attachment.expires_at > sqlc.arg(now)
              AND NOT EXISTS (
                SELECT 1
                FROM tunnel_certificate_edge_distributions AS edge_cert
                WHERE edge_cert.certificate_id = cert.id
                  AND edge_cert.edge_node_id = attachment.edge_node_id
                  AND edge_cert.edge_process_epoch = attachment.edge_process_epoch
                  AND edge_cert.edge_assignment_generation = attachment.attachment_generation
                  AND edge_cert.observed_certificate_generation = cert.certificate_generation
                  AND edge_cert.state = 'active'
                  AND edge_cert.updated_at >= cert.updated_at
              )
          )
      )
    )
  )
ORDER BY d.certificate_expires_at NULLS FIRST, d.id
LIMIT sqlc.arg(row_limit);

-- Current ready preview attachment tuples are the distribution targets for a
-- preview certificate. The attachment generation is used as the assignment
-- fence because preview carriers do not have durable route assignments.
-- name: ListReadyPreviewCertificateEdgesV1 :many
SELECT a.edge_node_id, a.edge_process_epoch, a.attachment_generation
FROM preview_lease_carrier_attachments AS a
JOIN preview_leases AS lease
  ON lease.id = a.preview_id AND lease.account_id = a.account_id
JOIN preview_domains AS d
  ON d.preview_id = lease.id AND d.account_id = lease.account_id
JOIN control_tunnel_nodes n ON n.id=a.edge_node_id AND n.process_epoch=a.edge_process_epoch
JOIN user_machines m ON m.id=a.host_id AND m.id=lease.owner_machine_id AND m.user_id=lease.account_id
WHERE d.id = sqlc.arg(domain_id)
  AND a.state IN ('pending','admitted','ready')
  AND a.lease_generation>0 AND a.lease_generation<=lease.generation
  AND a.owner_session_id=lease.owner_session_id
  AND a.machine_identity_public_key=m.public_identity_key
  AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND m.seat_state='occupied'
  AND m.state NOT IN ('pending','revoked','deleted')
  AND n.ready AND n.state='ready' AND n.drain_deadline IS NULL
  AND n.last_heartbeat_at>sqlc.arg(now)::timestamptz-interval '15 seconds'
  AND n.registry_expires_at>sqlc.arg(now)
  AND n.roles @> ARRAY['edge']::text[]
  AND paperboat.node_preview_eligible(n.id,lease.account_id,lease.id)
  AND d.ownership_state='verified' AND d.conflict_state='clear' AND d.deleted_at IS NULL
  AND (paperboat.workspace_edge_mode(lease.account_id,lease.owner_team_id)='managed-only' OR paperboat.selfhost_publication_hostname_allowed(d.hostname))
  AND a.expires_at > sqlc.arg(now)
  AND lease.terminal_state = 'active'
  AND (lease.owner_session_kind<>'lazy_runtime' OR EXISTS (SELECT 1 FROM lazy_access_policies policy WHERE policy.account_id=lease.account_id AND policy.machine_id=lease.owner_machine_id AND lease.endpoint='https://'||policy.hostname AND policy.target_scheme=lease.target_scheme AND policy.target_address=lease.target_address AND policy.access_mode=lease.access_mode AND policy.deleted_at IS NULL))
  AND lease.generation = d.preview_generation
  AND lease.lease_deadline > sqlc.arg(now)
  AND (lease.user_deadline IS NULL OR lease.user_deadline > sqlc.arg(now))
ORDER BY a.edge_node_id, a.edge_process_epoch, a.attachment_generation;

-- Authenticated first-SNI requests may issue an exact leaf only when the
-- verified preview wildcard parent and this edge's current ready attachment
-- are both present. A preview lease never falls back to a wildcard parent for
-- a matcher-marked exact name.
-- name: ListOnDemandPreviewDomainsForEdgeV1 :many
SELECT d.*
FROM preview_domains AS d
JOIN preview_leases AS lease
  ON lease.id = d.preview_id AND lease.account_id = d.account_id
JOIN preview_lease_carrier_attachments AS a
  ON a.account_id = d.account_id AND a.preview_id = d.preview_id
 AND a.lease_generation > 0 AND a.lease_generation <= d.preview_generation
 AND a.owner_session_id=lease.owner_session_id AND a.host_id=lease.owner_machine_id
WHERE d.certificate_strategy = 'on_demand_leaf'
  AND d.match_type = 'one_label_wildcard'
  AND d.ownership_state = 'verified'
  AND d.conflict_state = 'clear'
  AND d.certificate_state = 'ready'
  AND d.certificate_reference IS NOT NULL
  AND d.deleted_at IS NULL
  AND lease.terminal_state = 'active'
  AND (lease.owner_session_kind<>'lazy_runtime' OR EXISTS (SELECT 1 FROM lazy_access_policies policy WHERE policy.account_id=lease.account_id AND policy.machine_id=lease.owner_machine_id AND lease.endpoint='https://'||policy.hostname AND policy.target_scheme=lease.target_scheme AND policy.target_address=lease.target_address AND policy.access_mode=lease.access_mode AND policy.deleted_at IS NULL))
  AND lease.generation = d.preview_generation
  AND lease.lease_deadline > sqlc.arg(now)
  AND (lease.user_deadline IS NULL OR lease.user_deadline > sqlc.arg(now))
  AND a.edge_node_id = sqlc.arg(edge_node_id)
  AND a.edge_process_epoch = sqlc.arg(edge_process_epoch)
  AND a.state = 'ready'
  AND a.edge_ready
  AND a.expires_at > sqlc.arg(now)
  AND EXISTS (
    SELECT 1
    FROM tunnel_certificate_records AS cert
    WHERE cert.domain_id = d.id
      AND cert.account_id = d.account_id
      AND cert.target_kind = 'preview_lease'
      AND cert.preview_id = d.preview_id
      AND cert.preview_generation = d.preview_generation
      AND cert.certificate_reference = d.certificate_reference
      AND cert.leaf_hostname IS NULL
      AND cert.state = 'active'
      AND cert.expires_at > sqlc.arg(now)
  )
ORDER BY d.id
LIMIT sqlc.arg(row_limit);

-- name: MarkTunnelDomainCertificateReadyV1 :one
UPDATE tunnel_domains
SET certificate_state = 'ready',
    certificate_reference = sqlc.arg(certificate_reference),
    certificate_expires_at = sqlc.arg(certificate_expires_at),
    certificate_renewal_attempted_at = sqlc.arg(now),
    certificate_failure_code = NULL,
    caa_state = 'ready',
    generation = generation + 1,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(domain_id)
  AND generation = sqlc.arg(expected_generation)
  AND ownership_state = 'verified'
  AND conflict_state = 'clear'
  AND deleted_at IS NULL
RETURNING *;

-- name: MarkTunnelDomainCertificateFailureV1 :one
UPDATE tunnel_domains
SET certificate_state = CASE WHEN certificate_state = 'ready' THEN 'renewing' ELSE 'failed' END,
    certificate_renewal_attempted_at = sqlc.arg(now),
    certificate_failure_code = sqlc.arg(failure_code),
    generation = generation + 1,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(domain_id)
  AND generation = sqlc.arg(expected_generation)
  AND ownership_state = 'verified'
  AND deleted_at IS NULL
RETURNING *;
