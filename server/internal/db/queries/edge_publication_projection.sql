-- name: ListEdgeDNSPublicationResourcesV1 :many
-- Paginate resources, not rows/addresses, so a batch cannot publish a partial set.
SELECT id, max(generation)::bigint AS generation FROM (
 SELECT id,generation FROM tunnels
  WHERE desired_state='active' AND deleted_at IS NULL
   AND (expires_at IS NULL OR expires_at>statement_timestamp())
 UNION ALL SELECT id,1::bigint AS generation FROM preview_leases WHERE terminal_state='active'
 UNION ALL SELECT id,desired_revision AS generation FROM control_routes
  WHERE kind='runtime_https_wss' AND desired_state IN ('attached','replacing')
 UNION ALL SELECT owner_id AS id,resource_generation AS generation FROM edge_dns_publications
  WHERE state<>'verified' OR desired_addresses<>'[]'::jsonb OR provider_records<>'[]'::jsonb
) owners WHERE id > sqlc.arg(after_id)::text
GROUP BY id ORDER BY id LIMIT sqlc.arg(batch_size);

-- name: ListEdgeDNSPublicationNamesV1 :many
-- Managed endpoints and verified domain reservations share the tunnel's owner.
SELECT substring(t.stable_endpoint from 9)::text AS hostname
FROM tunnels t WHERE t.id=sqlc.arg(tunnel_id)
UNION
SELECT d.hostname FROM tunnel_domains d
WHERE d.tunnel_id=sqlc.arg(tunnel_id) AND d.ownership_state='verified'
 AND d.conflict_state='clear' AND d.deleted_at IS NULL
UNION
SELECT r.public_host FROM control_routes r WHERE r.id=sqlc.arg(tunnel_id)
 AND r.kind='runtime_https_wss' AND r.desired_state IN ('attached','replacing')
UNION
SELECT substring(p.endpoint from 9)::text FROM preview_leases p
WHERE p.id=sqlc.arg(tunnel_id) AND paperboat.workspace_edge_mode(p.account_id,p.owner_team_id)='managed-only'
ORDER BY hostname LIMIT 129;

-- name: ListReadyRuntimeEdgeDNSAddressesV1 :many
-- Every edge that holds an exact, freshly observed carrier of the route may receive a browser URL:
-- a machine keeps a carrier to each edge of its set and either one serves the host. The primary
-- (applied) carrier must exist, so a route that is not applied yet is advertised nowhere.
SELECT n.id, r.desired_revision AS resource_generation, statement_timestamp()::timestamptz AS observed_at,
 COALESCE(host(n.public_ingress_ipv4),'')::text AS ipv4,
 COALESCE(host(n.public_ingress_ipv6),'')::text AS ipv6,
 (n.process_epoch || '/' || a.session_id || '/' || a.generation::text)::text AS readiness_version,
 LEAST(a.expires_at,n.registry_expires_at,n.last_heartbeat_at+interval '30 seconds',a.observed_at+interval '30 seconds')::timestamptz AS valid_until
FROM control_routes r
JOIN runtime_carrier_attachments a ON a.route_id=r.id
JOIN control_tunnel_nodes n ON n.id=a.edge_node_id AND n.process_epoch=a.edge_process_epoch
WHERE r.id=sqlc.arg(route_id) AND r.public_host=sqlc.arg(hostname)
 AND r.applied_node_id IS NOT NULL AND r.applied_revision=r.desired_revision
 AND paperboat.runtime_carrier_valid(r.id,n.id,n.process_epoch,true)
 AND n.public_ingress_verified_at IS NOT NULL
 AND (n.public_ingress_ipv4 IS NOT NULL OR n.public_ingress_ipv6 IS NOT NULL)
ORDER BY n.id;

-- name: ListReadyEdgeDNSAddressesV1 :many
-- Every route in the serving config sharing the hostname must be usable on the advertised node.
-- An unapplied/unready replacement must not withdraw the serving hostname.
-- An active connector alone is not certificate, listener or origin readiness.
SELECT DISTINCT node.id,t.generation AS resource_generation,statement_timestamp()::timestamptz AS observed_at, COALESCE(host(node.public_ingress_ipv4),'')::text AS ipv4,
 COALESCE(host(node.public_ingress_ipv6),'')::text AS ipv6,
 (node.process_epoch || '/' || COALESCE((
   SELECT string_agg(a.assignment_id,',' ORDER BY a.assignment_id)
   FROM tunnel_edge_route_assignments a WHERE a.tunnel_id=t.id AND a.edge_node_id=node.id
    AND a.edge_process_epoch=node.process_epoch AND a.state='active' AND a.observed_state='ready'
 ),'') || '/' || COALESCE((
   SELECT string_agg(cert.id,',' ORDER BY cert.id)
   FROM tunnel_certificate_records cert JOIN tunnel_certificate_edge_distributions d ON d.certificate_id=cert.id
   WHERE cert.state='active' AND d.edge_node_id=node.id AND d.edge_process_epoch=node.process_epoch
    AND d.state IN ('ready','active') AND (cert.tunnel_id=t.id OR cert.target_kind='platform_wildcard')
 ),''))::text AS readiness_version,
 LEAST(t.expires_at,node.registry_expires_at,node.last_heartbeat_at+interval '15 seconds',
   (SELECT min(LEAST(s.lease_deadline,s.last_heartbeat_at+interval '15 seconds'))
    FROM tunnel_edge_route_assignments a JOIN tunnel_connector_sessions s ON s.id=a.connector_session_id
    WHERE a.tunnel_id=t.id AND a.edge_node_id=node.id AND a.edge_process_epoch=node.process_epoch
     AND a.state='active' AND a.observed_state='ready'))::timestamptz AS valid_until
FROM tunnels t
JOIN control_tunnel_nodes node ON true
WHERE t.id=sqlc.arg(tunnel_id) AND t.desired_state='active' AND t.deleted_at IS NULL
 AND (t.expires_at IS NULL OR t.expires_at>sqlc.arg(now))
 AND t.access_mode IN ('public','private','team')
 AND node.state='ready' AND node.ready AND node.drain_deadline IS NULL
 AND node.roles @> ARRAY['edge']::text[]
 AND node.last_heartbeat_at>sqlc.arg(now)::timestamptz-interval '15 seconds'
 AND node.registry_expires_at>sqlc.arg(now)
 AND node.public_ingress_verified_at IS NOT NULL
 AND (node.public_ingress_ipv4 IS NOT NULL OR node.public_ingress_ipv6 IS NOT NULL)
 AND EXISTS (SELECT 1 FROM tunnel_routes r WHERE r.tunnel_id=t.id AND EXISTS (
     SELECT 1 FROM tunnel_config_generations serving_config,
       LATERAL jsonb_array_elements(convert_from(serving_config.snapshot,'UTF8')::jsonb->'routes') serving_route
     WHERE serving_config.tunnel_id=t.id AND serving_config.activation_state='active' AND serving_route->>'id'=r.id
   ) AND r.desired_state='active' AND r.deleted_at IS NULL AND r.protocol IN ('http','tcp','tls')
   AND ((r.match_type IN ('managed','exact') AND r.match_hostname=sqlc.arg(hostname)::text)
     OR (r.match_type='one_label_wildcard' AND substring(sqlc.arg(hostname)::text from position('.' in sqlc.arg(hostname)::text))='.'||r.wildcard_suffix)
     OR EXISTS (SELECT 1 FROM tunnel_domains domain WHERE domain.route_id=r.id AND domain.tunnel_id=r.tunnel_id AND domain.hostname=sqlc.arg(hostname)::text AND domain.deleted_at IS NULL AND domain.ownership_state='verified' AND domain.conflict_state='clear')))
 AND NOT EXISTS (
  SELECT 1 FROM tunnel_routes r
  WHERE r.tunnel_id=t.id AND EXISTS (
     SELECT 1 FROM tunnel_config_generations serving_config,
       LATERAL jsonb_array_elements(convert_from(serving_config.snapshot,'UTF8')::jsonb->'routes') serving_route
     WHERE serving_config.tunnel_id=t.id AND serving_config.activation_state='active' AND serving_route->>'id'=r.id
   ) AND r.desired_state='active' AND r.deleted_at IS NULL AND r.protocol IN ('http','tcp','tls')
   AND ((r.match_type IN ('managed','exact') AND r.match_hostname=sqlc.arg(hostname)::text)
     OR (r.match_type='one_label_wildcard' AND substring(sqlc.arg(hostname)::text from position('.' in sqlc.arg(hostname)::text))='.'||r.wildcard_suffix)
     OR EXISTS (SELECT 1 FROM tunnel_domains domain WHERE domain.route_id=r.id AND domain.tunnel_id=r.tunnel_id AND domain.hostname=sqlc.arg(hostname)::text AND domain.deleted_at IS NULL AND domain.ownership_state='verified' AND domain.conflict_state='clear'))
   AND NOT EXISTS (
    SELECT 1 FROM tunnel_edge_route_assignments a
    JOIN tunnel_connectors c ON c.id=a.connector_id AND c.tunnel_id=t.id
    JOIN tunnel_connector_sessions s ON s.id=a.connector_session_id AND s.connector_id=c.id
    JOIN tunnel_config_generations config ON config.tunnel_id=t.id AND config.generation=a.config_generation
    JOIN current_tunnel_route_viewer_policy viewer_policy ON viewer_policy.route_id=r.id
    WHERE a.route_id=r.id AND a.route_generation=r.generation AND a.account_id=t.account_id
     AND paperboat.node_route_eligible(node.id,t.account_id,r.id)
     AND a.state='active' AND a.observed_state='ready'
     AND a.access_mode=viewer_policy.effective_access_mode
     AND a.viewer_policy_generation=viewer_policy.viewer_policy_generation
     AND a.edge_node_id=node.id AND a.edge_process_epoch=node.process_epoch
     AND c.desired_state='active' AND c.drain_state='accepting' AND c.revoked_at IS NULL
     AND c.generation=a.connector_generation AND c.last_session_id=s.id
     AND c.last_applied_config_generation>=a.config_generation
     AND s.state='ready' AND s.process_generation=a.connector_process_generation
     AND s.applied_config_generation>=a.config_generation AND s.lease_deadline>sqlc.arg(now)
     AND s.last_heartbeat_at>sqlc.arg(now)::timestamptz-interval '15 seconds'
     AND config.activation_state='active' AND config.content_hash=a.config_content_hash
   )
 )
 AND (
  NOT EXISTS (SELECT 1 FROM tunnel_routes r WHERE r.tunnel_id=t.id AND EXISTS (
     SELECT 1 FROM tunnel_config_generations serving_config,
       LATERAL jsonb_array_elements(convert_from(serving_config.snapshot,'UTF8')::jsonb->'routes') serving_route
     WHERE serving_config.tunnel_id=t.id AND serving_config.activation_state='active' AND serving_route->>'id'=r.id
   ) AND r.desired_state='active' AND r.deleted_at IS NULL AND r.protocol='http'
    AND ((r.match_type IN ('managed','exact') AND r.match_hostname=sqlc.arg(hostname)::text) OR (r.match_type='one_label_wildcard' AND substring(sqlc.arg(hostname)::text from position('.' in sqlc.arg(hostname)::text))='.'||r.wildcard_suffix)
      OR EXISTS (SELECT 1 FROM tunnel_domains domain WHERE domain.route_id=r.id AND domain.tunnel_id=r.tunnel_id AND domain.hostname=sqlc.arg(hostname)::text AND domain.deleted_at IS NULL AND domain.ownership_state='verified' AND domain.conflict_state='clear')))
  OR EXISTS (
   SELECT 1 FROM tunnel_certificate_records cert
   JOIN tunnel_certificate_edge_distributions distribution ON distribution.certificate_id=cert.id
   WHERE cert.state='active' AND cert.revoked_at IS NULL AND cert.not_before<=sqlc.arg(now) AND cert.expires_at>sqlc.arg(now)
    AND (cert.tunnel_id=t.id OR cert.target_kind='platform_wildcard')
    AND (cert.hostname=sqlc.arg(hostname) OR
      (left(cert.hostname,2)='*.' AND substring(sqlc.arg(hostname)::text from position('.' in sqlc.arg(hostname)::text))=substring(cert.hostname from 2)))
    AND distribution.edge_node_id=node.id AND distribution.edge_process_epoch=node.process_epoch
    AND distribution.state IN ('ready','active')
    AND distribution.observed_certificate_generation=cert.certificate_generation
  )
 )
UNION ALL
-- Preview IDs and hostnames are immutable; lease/attachment renewal counters
-- must not rewrite DNS. Placement and process changes fence readiness instead.
-- A preview advertises exactly the edges that hold one of its live carrier attachments, from the
-- moment the attachment is allocated, so the record is written before the carrier connects.
SELECT node.id,1::bigint AS resource_generation,statement_timestamp()::timestamptz AS observed_at,
 COALESCE(host(node.public_ingress_ipv4),'')::text AS ipv4,
 COALESCE(host(node.public_ingress_ipv6),'')::text AS ipv6,
 (node.process_epoch||'/'||a.operation_id||'/'||a.route_id||'/'||a.route_generation::text||'/'||a.connector_session_id||'/'||a.process_generation::text||'/'||cert.id)::text AS readiness_version,
 LEAST(p.lease_deadline,p.user_deadline,a.expires_at,identity.expires_at,node.registry_expires_at,node.last_heartbeat_at+interval '15 seconds',cert.expires_at)::timestamptz AS valid_until
FROM preview_leases p
JOIN preview_lease_carrier_attachments a ON a.preview_id=p.id AND a.account_id=p.account_id
JOIN user_machines m ON m.id=a.host_id AND m.id=p.owner_machine_id AND m.user_id=p.account_id
JOIN users owner ON owner.id=p.account_id AND owner.status='active'
JOIN control_environments env ON env.id=m.environment_id AND env.desired_state='active' AND env.revoked_at IS NULL
JOIN machine_control_sessions identity ON identity.machine_id=m.id AND identity.installation_generation=m.installation_generation
JOIN control_tunnel_nodes node ON node.id=a.edge_node_id AND node.process_epoch=a.edge_process_epoch
JOIN tunnel_certificate_edge_distributions distribution ON distribution.edge_node_id=node.id AND distribution.edge_process_epoch=node.process_epoch AND distribution.state IN ('ready','active')
JOIN tunnel_certificate_records cert ON cert.id=distribution.certificate_id AND cert.target_kind='platform_wildcard'
 AND cert.certificate_generation=distribution.observed_certificate_generation
WHERE p.id=sqlc.arg(tunnel_id) AND substring(p.endpoint from 9)=sqlc.arg(hostname)
 AND paperboat.workspace_edge_mode(p.account_id,p.owner_team_id)='managed-only'
 AND p.terminal_state='active' AND p.lease_deadline>sqlc.arg(now) AND (p.user_deadline IS NULL OR p.user_deadline>sqlc.arg(now))
 AND a.state IN ('pending','admitted','edge_ready','ready') AND a.expires_at>sqlc.arg(now)
 AND a.lease_generation>0 AND a.lease_generation<=p.generation
 AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND m.seat_state='occupied' AND m.state NOT IN ('pending','revoked','deleted')
 AND m.public_identity_key=a.machine_identity_public_key AND identity.issued_at<=sqlc.arg(now) AND identity.expires_at>sqlc.arg(now)
 AND (p.owner_team_id IS NULL OR (paperboat.team_paid_entitled(p.owner_team_id) AND paperboat.workspace_preview_management_allowed(p.account_id,p.owner_team_id,p.id)))
 AND paperboat.node_preview_eligible(node.id,p.account_id,p.id)
 AND node.state='ready' AND node.ready AND node.drain_deadline IS NULL AND node.roles @> ARRAY['edge']::text[]
 AND node.registry_expires_at>sqlc.arg(now) AND node.last_heartbeat_at>sqlc.arg(now)::timestamptz-interval '15 seconds'
 AND node.public_ingress_verified_at IS NOT NULL AND (node.public_ingress_ipv4 IS NOT NULL OR node.public_ingress_ipv6 IS NOT NULL)
 AND cert.state='active' AND cert.revoked_at IS NULL AND cert.not_before<=sqlc.arg(now) AND cert.expires_at>sqlc.arg(now)
 AND left(cert.hostname,2)='*.' AND substring(sqlc.arg(hostname)::text from position('.' in sqlc.arg(hostname)::text))=substring(cert.hostname from 2)
UNION ALL
-- Until the host attaches (seconds after creation) the record already points at the edge that will hold
-- the carrier, the machine's primary one: the URL is never known without its record. A resolver that looked
-- the name up earlier would cache the shared wildcard answer, which can be the other edge, for its TTL.
SELECT node.id,1::bigint,statement_timestamp()::timestamptz,
 COALESCE(host(node.public_ingress_ipv4),'')::text,
 COALESCE(host(node.public_ingress_ipv6),'')::text,
 (node.process_epoch||'/intent/'||p.id||'/'||cert.id)::text,
 LEAST(p.lease_deadline,p.user_deadline,p.created_at+interval '2 minutes',node.registry_expires_at,node.last_heartbeat_at+interval '15 seconds',cert.expires_at)::timestamptz
FROM preview_leases p
JOIN user_machines m ON m.id=p.owner_machine_id AND m.user_id=p.account_id
JOIN runtime_carrier_attachments carrier ON carrier.machine_id=m.id AND carrier.account_id=p.account_id
 AND carrier.installation_generation=m.installation_generation AND carrier.active AND carrier.ready
 AND paperboat.runtime_carrier_valid(carrier.route_id,carrier.edge_node_id,carrier.edge_process_epoch,true)
JOIN control_tunnel_nodes node ON node.id=carrier.edge_node_id AND node.process_epoch=carrier.edge_process_epoch
JOIN tunnel_certificate_edge_distributions distribution ON distribution.edge_node_id=node.id AND distribution.edge_process_epoch=node.process_epoch AND distribution.state IN ('ready','active')
JOIN tunnel_certificate_records cert ON cert.id=distribution.certificate_id AND cert.target_kind='platform_wildcard'
 AND cert.certificate_generation=distribution.observed_certificate_generation
WHERE p.id=sqlc.arg(tunnel_id) AND substring(p.endpoint from 9)=sqlc.arg(hostname)
 AND paperboat.workspace_edge_mode(p.account_id,p.owner_team_id)='managed-only'
 AND p.terminal_state='active' AND p.lease_deadline>sqlc.arg(now) AND (p.user_deadline IS NULL OR p.user_deadline>sqlc.arg(now))
 AND p.created_at>sqlc.arg(now)::timestamptz-interval '2 minutes'
 AND NOT EXISTS (SELECT 1 FROM preview_lease_carrier_attachments live WHERE live.account_id=p.account_id AND live.preview_id=p.id AND live.state NOT IN ('failed','released'))
 AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND m.seat_state='occupied' AND m.state NOT IN ('pending','revoked','deleted')
 AND (p.owner_team_id IS NULL OR (paperboat.team_paid_entitled(p.owner_team_id) AND paperboat.workspace_preview_management_allowed(p.account_id,p.owner_team_id,p.id)))
 AND paperboat.node_preview_eligible(node.id,p.account_id,p.id)
 AND node.state='ready' AND node.ready AND node.drain_deadline IS NULL AND node.roles @> ARRAY['edge']::text[]
 AND node.registry_expires_at>sqlc.arg(now) AND node.last_heartbeat_at>sqlc.arg(now)::timestamptz-interval '15 seconds'
 AND node.public_ingress_verified_at IS NOT NULL AND (node.public_ingress_ipv4 IS NOT NULL OR node.public_ingress_ipv6 IS NOT NULL)
 AND cert.state='active' AND cert.revoked_at IS NULL AND cert.not_before<=sqlc.arg(now) AND cert.expires_at>sqlc.arg(now)
 AND left(cert.hostname,2)='*.' AND substring(sqlc.arg(hostname)::text from position('.' in sqlc.arg(hostname)::text))=substring(cert.hostname from 2)
ORDER BY id LIMIT 3;

-- name: TransferWithdrawnEdgeDNSNameV1 :execrows
-- A new resource may acquire a released hostname only after verified removal
-- and only while the authoritative namespace currently belongs to it. Keep
-- the publication generation monotonic to fence delayed old-owner cleanup.
UPDATE edge_dns_publications p
SET owner_id=sqlc.arg(owner_id),resource_generation=sqlc.arg(resource_generation),
 publication_generation=publication_generation+1,readiness_version='',
 observed_at=sqlc.arg(observed_at),valid_until=NULL,state='desired',failure_code=NULL,
 verified_at=NULL,updated_at=statement_timestamp()
WHERE p.hostname=sqlc.arg(hostname) AND p.owner_id<>sqlc.arg(owner_id)
 AND p.state='verified' AND p.desired_addresses='[]'::jsonb AND p.provider_records='[]'::jsonb
 AND p.observed_at<=sqlc.arg(observed_at)
 AND (
  EXISTS (SELECT 1 FROM tunnels t WHERE t.id=sqlc.arg(owner_id) AND t.generation=sqlc.arg(resource_generation)
    AND t.deleted_at IS NULL AND t.desired_state<>'deleted' AND substring(t.stable_endpoint from 9)=p.hostname)
  OR EXISTS (SELECT 1 FROM tunnel_domains d JOIN tunnels t ON t.id=d.tunnel_id
    WHERE d.tunnel_id=sqlc.arg(owner_id) AND d.hostname=p.hostname AND d.deleted_at IS NULL
      AND d.ownership_state='verified' AND d.conflict_state='clear' AND t.deleted_at IS NULL
      AND t.desired_state<>'deleted' AND t.generation=sqlc.arg(resource_generation))
 );
