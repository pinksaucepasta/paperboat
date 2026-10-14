-- +goose Up
-- +goose StatementBegin
ALTER TABLE paperboat.selfhost_pool_policies DROP CONSTRAINT selfhost_pool_account_scope;
ALTER TABLE paperboat.selfhost_pool_policies DROP CONSTRAINT selfhost_pool_policies_mode_check;
UPDATE paperboat.selfhost_pool_policies SET mode='managed-only',installation_ids='{}' WHERE capability='tunnel' AND mode='mixed';
ALTER TABLE paperboat.selfhost_pool_policies ADD CONSTRAINT selfhost_pool_modes CHECK (
 (capability='relay' AND scope_kind='account' AND mode IN('mixed','self-hosted-only')) OR
 (capability='tunnel' AND mode IN('managed-only','self-hosted-only')));
ALTER TABLE paperboat.selfhost_pool_policies ADD CONSTRAINT managed_edges_no_installations CHECK(mode<>'managed-only' OR cardinality(installation_ids)=0);
ALTER TABLE paperboat.control_routes ADD COLUMN managed_public_host text;
UPDATE paperboat.control_routes SET managed_public_host=public_host WHERE kind='runtime_https_wss';
CREATE FUNCTION paperboat.remember_managed_runtime_host() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.kind='runtime_https_wss' AND NEW.managed_public_host IS NULL THEN NEW.managed_public_host=NEW.public_host; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER remember_managed_runtime_host BEFORE INSERT ON paperboat.control_routes FOR EACH ROW EXECUTE FUNCTION paperboat.remember_managed_runtime_host();
CREATE FUNCTION paperboat.workspace_edge_mode(wanted_account text,wanted_team text) RETURNS text LANGUAGE sql STABLE AS $$
 SELECT coalesce((SELECT mode FROM paperboat.selfhost_pool_policies WHERE capability='tunnel'
 AND scope_kind=CASE WHEN wanted_team IS NULL THEN 'account' ELSE 'team' END
 AND scope_id=coalesce(wanted_team,wanted_account)),'managed-only');
$$;
CREATE FUNCTION paperboat.publication_workspace_team(wanted_kind text,wanted_resource text) RETURNS text LANGUAGE sql STABLE AS $$
 SELECT CASE WHEN wanted_kind='preview' THEN (SELECT owner_team_id FROM paperboat.preview_leases WHERE id=wanted_resource)
 WHEN wanted_kind='tunnel' THEN (SELECT owner_team_id FROM paperboat.tunnels WHERE id=wanted_resource) END;
$$;
CREATE FUNCTION paperboat.selfhost_publication_hostname_allowed(wanted_hostname text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT wanted_hostname IS NOT NULL AND NOT EXISTS (
 SELECT 1 FROM paperboat.tunnel_platform_certificate_targets platform
 WHERE lower(wanted_hostname)=lower(platform.hostname)
 OR (left(platform.hostname,2)='*.' AND (lower(wanted_hostname)=lower(substring(platform.hostname from 3))
 OR right(lower(wanted_hostname),length(platform.hostname)-1)=lower(substring(platform.hostname from 2))))
 ) AND NOT EXISTS (
 SELECT 1 FROM paperboat.control_tunnel_nodes node
 WHERE lower(node.endpoint_host)=lower(wanted_hostname)
 AND NOT EXISTS (SELECT 1 FROM paperboat.selfhost_installations installation WHERE installation.node_id=node.id AND installation.scope_kind<>'global')
 );
$$;
CREATE FUNCTION paperboat.node_workspace_edge_eligible(wanted_node text,wanted_account text,wanted_team text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.control_tunnel_nodes n
 LEFT JOIN paperboat.managed_node_authority m ON m.node_id=n.id
 LEFT JOIN paperboat.operator_regions region ON region.id=m.region_id
 WHERE n.id=wanted_node AND (wanted_team IS NULL OR EXISTS(SELECT 1 FROM paperboat.teams t JOIN paperboat.team_members member USING(team_id) WHERE t.team_id=wanted_team AND t.deleted_at IS NULL AND NOT t.suspended AND member.account_id=wanted_account AND member.active)) AND (
 (paperboat.workspace_edge_mode(wanted_account,wanted_team)='managed-only'
 AND NOT EXISTS(SELECT 1 FROM paperboat.selfhost_installations i WHERE i.node_id=n.id AND i.scope_kind<>'global')
 AND (m.node_id IS NULL OR(m.retired_at IS NULL AND region.enabled))
 AND(n.allowed_account_ids IS NULL OR wanted_account=ANY(n.allowed_account_ids))) OR
 (paperboat.workspace_edge_mode(wanted_account,wanted_team)='self-hosted-only' AND EXISTS(
 SELECT 1 FROM paperboat.selfhost_installations i JOIN paperboat.selfhost_pool_policies p ON i.id=ANY(p.installation_ids)
 WHERE i.node_id=n.id AND i.capability='tunnel' AND i.state='active' AND i.claim_complete
 AND i.scope_kind<>'global' AND octet_length(n.tls_spki_sha256)=32
 AND p.capability='tunnel' AND p.mode='self-hosted-only'
 AND p.scope_kind=CASE WHEN wanted_team IS NULL THEN 'account' ELSE 'team' END AND p.scope_id=coalesce(wanted_team,wanted_account)
 AND ((wanted_team IS NULL AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,wanted_account)) OR(i.scope_kind='team' AND i.scope_id=wanted_team))
 ))));
$$;
-- Relay behavior is unchanged. Tunnel consumers without a resource use Personal.
CREATE OR REPLACE FUNCTION paperboat.node_pool_eligible(wanted_node text,wanted_account text,wanted_capability text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT CASE WHEN wanted_capability='tunnel' THEN paperboat.node_workspace_edge_eligible(wanted_node,wanted_account,NULL)
 WHEN wanted_capability='relay' THEN EXISTS(SELECT 1 FROM paperboat.control_tunnel_nodes n
 LEFT JOIN paperboat.selfhost_installations i ON i.node_id=n.id
 LEFT JOIN paperboat.managed_node_authority m ON m.node_id=n.id
 LEFT JOIN paperboat.operator_regions r ON r.id=m.region_id
 LEFT JOIN paperboat.selfhost_pool_policies p ON p.scope_kind='account' AND p.scope_id=wanted_account AND p.capability='relay'
 WHERE n.id=wanted_node AND (
 ((i.id IS NULL OR(i.scope_kind='global' AND i.state='active' AND i.claim_complete AND octet_length(n.tls_spki_sha256)=32)) AND(m.node_id IS NULL OR(m.retired_at IS NULL AND r.enabled)) AND coalesce(p.mode,'mixed')='mixed' AND(n.allowed_account_ids IS NULL OR wanted_account=ANY(n.allowed_account_ids))) OR
 (i.scope_kind<>'global' AND i.state='active' AND i.claim_complete AND octet_length(n.tls_spki_sha256)=32 AND i.capability='relay' AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,wanted_account) AND(p.scope_id IS NULL OR i.id=ANY(p.installation_ids))))) ELSE false END;
$$;
CREATE FUNCTION paperboat.publication_edge_nodes(wanted_kind text,wanted_resource text) RETURNS TABLE(node_id text) LANGUAGE sql STABLE AS $$
 WITH resource AS (
 SELECT account_id,paperboat.publication_workspace_team(wanted_kind,wanted_resource) AS team FROM paperboat.preview_leases WHERE wanted_kind='preview' AND id=wanted_resource
 UNION ALL SELECT account_id,paperboat.publication_workspace_team(wanted_kind,wanted_resource) FROM paperboat.tunnels WHERE wanted_kind='tunnel' AND id=wanted_resource), candidates AS (
 SELECT DISTINCT ON(n.failure_domain) n.id,n.failure_domain,EXISTS(SELECT 1 FROM (SELECT expected_records FROM paperboat.preview_domains WHERE wanted_kind='preview' AND preview_id=wanted_resource AND deleted_at IS NULL AND ownership_state='verified' AND conflict_state='clear' UNION ALL SELECT expected_records FROM paperboat.tunnel_domains WHERE wanted_kind='tunnel' AND tunnel_id=wanted_resource AND deleted_at IS NULL AND ownership_state='verified' AND conflict_state='clear') domain CROSS JOIN LATERAL jsonb_array_elements(domain.expected_records) record WHERE record->>'value' IN(host(n.public_ingress_ipv4),host(n.public_ingress_ipv6))) AS retained FROM resource r CROSS JOIN paperboat.control_tunnel_nodes n
 WHERE paperboat.node_workspace_edge_eligible(n.id,r.account_id,r.team)
 AND n.ready AND n.state='ready' AND n.drain_deadline IS NULL AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now()
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.public_ingress_verified_at IS NOT NULL AND(n.public_ingress_ipv4 IS NOT NULL OR n.public_ingress_ipv6 IS NOT NULL)
 AND (NOT EXISTS(SELECT 1 FROM paperboat.selfhost_placements WHERE resource_kind=wanted_kind AND resource_id=wanted_resource)
 OR EXISTS(SELECT 1 FROM paperboat.selfhost_placements p JOIN paperboat.selfhost_installations i ON i.id=p.installation_id WHERE p.resource_kind=wanted_kind AND p.resource_id=wanted_resource AND i.node_id=n.id))
 ORDER BY n.failure_domain,retained DESC,n.id)
 SELECT id FROM candidates ORDER BY retained DESC,id LIMIT CASE WHEN wanted_kind='preview' THEN 1 ELSE 2 END;
$$;
CREATE OR REPLACE FUNCTION paperboat.node_preview_eligible(wanted_node text,wanted_account text,wanted_preview text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.preview_leases p JOIN paperboat.control_tunnel_nodes ingress ON ingress.id=wanted_node WHERE p.id=wanted_preview AND p.account_id=wanted_account
 AND paperboat.node_workspace_edge_eligible(wanted_node,wanted_account,p.owner_team_id)
 AND(NOT EXISTS(SELECT 1 FROM paperboat.selfhost_placements WHERE resource_kind='preview' AND resource_id=p.id) OR EXISTS(SELECT 1 FROM paperboat.selfhost_placements placement JOIN paperboat.selfhost_installations i ON i.id=placement.installation_id WHERE placement.resource_kind='preview' AND placement.resource_id=p.id AND i.node_id=wanted_node))
 AND(paperboat.workspace_edge_mode(wanted_account,p.owner_team_id)='managed-only' OR (wanted_node IN(SELECT node_id FROM paperboat.publication_edge_nodes('preview',p.id)) AND EXISTS(SELECT 1 FROM paperboat.preview_domains d WHERE d.preview_id=p.id AND d.account_id=wanted_account AND d.deleted_at IS NULL AND d.ownership_state='verified' AND d.conflict_state='clear' AND paperboat.selfhost_publication_hostname_allowed(d.hostname) AND (ingress.public_ingress_ipv4 IS NOT NULL OR ingress.public_ingress_ipv6 IS NOT NULL) AND (ingress.public_ingress_ipv4 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='A' AND record->>'value'=host(ingress.public_ingress_ipv4))) AND (ingress.public_ingress_ipv6 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='AAAA' AND record->>'value'=host(ingress.public_ingress_ipv6)))))));
$$;
CREATE OR REPLACE FUNCTION paperboat.node_route_eligible(wanted_node text,wanted_account text,wanted_route text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.tunnel_routes route JOIN paperboat.tunnels t ON t.id=route.tunnel_id JOIN paperboat.control_tunnel_nodes ingress ON ingress.id=wanted_node WHERE route.id=wanted_route AND t.account_id=wanted_account
 AND paperboat.node_workspace_edge_eligible(wanted_node,wanted_account,paperboat.publication_workspace_team('tunnel',t.id))
 AND(NOT EXISTS(SELECT 1 FROM paperboat.selfhost_placements WHERE resource_kind='tunnel' AND resource_id=t.id) OR EXISTS(SELECT 1 FROM paperboat.selfhost_placements placement JOIN paperboat.selfhost_installations i ON i.id=placement.installation_id WHERE placement.resource_kind='tunnel' AND placement.resource_id=t.id AND i.node_id=wanted_node))
 AND(paperboat.workspace_edge_mode(wanted_account,paperboat.publication_workspace_team('tunnel',t.id))='managed-only' OR (wanted_node IN(SELECT node_id FROM paperboat.publication_edge_nodes('tunnel',t.id)) AND (route.protocol NOT IN('http','tls') OR EXISTS(SELECT 1 FROM paperboat.tunnel_domains d WHERE d.route_id=route.id AND d.account_id=wanted_account AND d.deleted_at IS NULL AND d.ownership_state='verified' AND d.conflict_state='clear' AND paperboat.selfhost_publication_hostname_allowed(d.hostname) AND (ingress.public_ingress_ipv4 IS NOT NULL OR ingress.public_ingress_ipv6 IS NOT NULL) AND (ingress.public_ingress_ipv4 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='A' AND record->>'value'=host(ingress.public_ingress_ipv4))) AND (ingress.public_ingress_ipv6 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='AAAA' AND record->>'value'=host(ingress.public_ingress_ipv6))))))));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.runtime_carrier_current(wanted_route text, require_ready boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
 SELECT EXISTS (
 SELECT 1 FROM runtime_carrier_attachments a
 JOIN user_machines m ON m.id=a.machine_id
 JOIN control_routes r ON r.id=a.route_id
 JOIN control_environments e ON e.id=r.environment_id
 JOIN control_tunnel_nodes n ON n.id=a.edge_node_id
 JOIN users u ON u.id=a.account_id
 WHERE a.route_id=wanted_route AND a.expires_at>now()
 AND m.user_id=a.account_id AND u.status='active'
 AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.seat_state='occupied'
  AND m.installation_generation=a.installation_generation AND m.worker_generation=a.worker_generation
 AND m.public_identity_key=a.public_identity_key
 AND r.environment_id=m.environment_id AND r.desired_revision=a.route_revision
 AND r.kind='runtime_https_wss' AND r.desired_state IN ('attached','replacing') AND e.desired_state='active'
 AND n.ready AND n.state='ready' AND n.process_epoch=a.edge_process_epoch
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.last_heartbeat_at>now()-interval '30 seconds' AND n.registry_expires_at>now()
 AND (n.drain_deadline IS NULL OR n.drain_deadline>now())
 AND paperboat.node_workspace_edge_eligible(n.id,a.account_id,m.owner_team_id)
 AND (CASE WHEN paperboat.workspace_edge_mode(a.account_id,m.owner_team_id)='managed-only' THEN r.public_host=r.managed_public_host ELSE r.public_host<>r.managed_public_host END)
 AND (NOT require_ready OR (a.ready AND a.observed_at>now()-interval '30 seconds'))
 );
$$;
-- +goose StatementEnd


-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.runtime_carrier_valid(wanted_route text,wanted_node text,wanted_epoch text,require_ready boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
 SELECT EXISTS (
 SELECT 1 FROM runtime_carrier_attachments a
 JOIN user_machines m ON m.id=a.machine_id
 JOIN control_routes r ON r.id=a.route_id
 JOIN control_environments e ON e.id=r.environment_id
 JOIN control_tunnel_nodes n ON n.id=a.edge_node_id
 JOIN users u ON u.id=a.account_id
 WHERE a.route_id=wanted_route AND a.edge_node_id=wanted_node AND a.edge_process_epoch=wanted_epoch
 AND a.expires_at>now() AND m.user_id=a.account_id AND u.status='active'
 AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.seat_state='occupied'
 AND m.installation_generation=a.installation_generation AND m.worker_generation=a.worker_generation
 AND m.public_identity_key=a.public_identity_key
 AND r.environment_id=m.environment_id AND r.desired_revision=a.route_revision
 AND r.kind='runtime_https_wss' AND r.desired_state IN ('attached','replacing') AND e.desired_state='active'
 AND n.ready AND n.state='ready' AND n.process_epoch=a.edge_process_epoch
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.last_heartbeat_at>now()-interval '30 seconds' AND n.registry_expires_at>now()
 AND (n.drain_deadline IS NULL OR n.drain_deadline>now())
 AND paperboat.node_workspace_edge_eligible(n.id,a.account_id,m.owner_team_id)
 AND (CASE WHEN paperboat.workspace_edge_mode(a.account_id,m.owner_team_id)='managed-only' THEN r.public_host=r.managed_public_host ELSE r.public_host<>r.managed_public_host END)
 AND (NOT require_ready OR (a.ready AND a.observed_at>now()-interval '30 seconds'))
 );
$$;
-- +goose StatementEnd

