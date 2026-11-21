-- +goose Up
-- One owner for "this hostname is verified for this workspace": workspace_domains. Tunnel and preview domain
-- bindings stop keeping their own ownership_state copy; every reader asks workspace_domain_verified() instead.
-- The binding columns ownership_state and last_verified_at are dropped one deploy later (post-deploy migration).
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_domain_verified(wanted_id text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.workspace_domains WHERE id=wanted_id AND ownership_state='verified');
$$;
-- +goose StatementEnd

-- A lapse or re-verification still bumps each live binding's generation: certificate staging and activation fence on it.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.workspace_domain_bindings_follow_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE paperboat.tunnel_domains SET generation = generation + 1, updated_at = now()
    WHERE workspace_domain_id = NEW.id AND deleted_at IS NULL;
    UPDATE paperboat.preview_domains SET generation = generation + 1, updated_at = now()
    WHERE workspace_domain_id = NEW.id AND deleted_at IS NULL;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.workspace_domain_bindings_withdraw() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE paperboat.tunnel_domains SET
        certificate_state = CASE WHEN certificate_state = 'not_applicable' THEN 'not_applicable' ELSE 'revoked' END,
        conflict_state = 'quarantined', quarantine_until = now(), -- already over: the next owner may bind at once
        deleted_at = now(), generation = generation + 1, updated_at = now()
    WHERE workspace_domain_id = OLD.id AND deleted_at IS NULL;
    UPDATE paperboat.preview_domains SET
        certificate_state = CASE WHEN certificate_state = 'not_applicable' THEN 'not_applicable' ELSE 'revoked' END,
        conflict_state = 'quarantined', quarantine_until = now(), -- already over: the next owner may bind at once
        deleted_at = now(), generation = generation + 1, updated_at = now()
    WHERE workspace_domain_id = OLD.id AND deleted_at IS NULL;
    RETURN OLD;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.node_preview_eligible(wanted_node text,wanted_account text,wanted_preview text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.preview_leases p JOIN paperboat.control_tunnel_nodes ingress ON ingress.id=wanted_node WHERE p.id=wanted_preview AND p.account_id=wanted_account
 AND paperboat.node_workspace_edge_eligible(wanted_node,wanted_account,p.owner_team_id)
 AND(NOT EXISTS(SELECT 1 FROM paperboat.selfhost_placements WHERE resource_kind='preview' AND resource_id=p.id) OR EXISTS(SELECT 1 FROM paperboat.selfhost_placements placement JOIN paperboat.selfhost_installations i ON i.id=placement.installation_id WHERE placement.resource_kind='preview' AND placement.resource_id=p.id AND i.node_id=wanted_node))
 AND(paperboat.workspace_edge_mode(wanted_account,p.owner_team_id)='managed-only' OR (wanted_node IN(SELECT node_id FROM paperboat.publication_edge_nodes('preview',p.id)) AND EXISTS(SELECT 1 FROM paperboat.preview_domains d WHERE d.preview_id=p.id AND d.account_id=wanted_account AND d.deleted_at IS NULL AND paperboat.workspace_domain_verified(d.workspace_domain_id) AND d.conflict_state='clear' AND paperboat.selfhost_publication_hostname_allowed(d.hostname) AND (ingress.public_ingress_ipv4 IS NOT NULL OR ingress.public_ingress_ipv6 IS NOT NULL) AND (ingress.public_ingress_ipv4 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='A' AND record->>'value'=host(ingress.public_ingress_ipv4))) AND (ingress.public_ingress_ipv6 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='AAAA' AND record->>'value'=host(ingress.public_ingress_ipv6)))))));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.node_route_eligible(wanted_node text, wanted_account text, wanted_route text)
 RETURNS boolean
 LANGUAGE plpgsql
 STABLE
 SET plan_cache_mode TO 'force_generic_plan'
AS $function$
BEGIN
 RETURN (SELECT EXISTS(SELECT 1 FROM paperboat.tunnel_routes route JOIN paperboat.tunnels t ON t.id=route.tunnel_id JOIN paperboat.control_tunnel_nodes ingress ON ingress.id=wanted_node WHERE route.id=wanted_route AND t.account_id=wanted_account
 AND paperboat.node_workspace_edge_eligible(wanted_node,wanted_account,paperboat.publication_workspace_team('tunnel',t.id))
 AND(NOT EXISTS(SELECT 1 FROM paperboat.selfhost_placements WHERE resource_kind='tunnel' AND resource_id=t.id) OR EXISTS(SELECT 1 FROM paperboat.selfhost_placements placement JOIN paperboat.selfhost_installations i ON i.id=placement.installation_id WHERE placement.resource_kind='tunnel' AND placement.resource_id=t.id AND i.node_id=wanted_node))
 AND(paperboat.workspace_edge_mode(wanted_account,paperboat.publication_workspace_team('tunnel',t.id))='managed-only' OR (wanted_node IN(SELECT node_id FROM paperboat.publication_edge_nodes('tunnel',t.id)) AND (route.protocol NOT IN('http','tls') OR EXISTS(SELECT 1 FROM paperboat.tunnel_domains d WHERE d.route_id=route.id AND d.account_id=wanted_account AND d.deleted_at IS NULL AND paperboat.workspace_domain_verified(d.workspace_domain_id) AND d.conflict_state='clear' AND paperboat.selfhost_publication_hostname_allowed(d.hostname) AND (ingress.public_ingress_ipv4 IS NOT NULL OR ingress.public_ingress_ipv6 IS NOT NULL) AND (ingress.public_ingress_ipv4 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='A' AND record->>'value'=host(ingress.public_ingress_ipv4))) AND (ingress.public_ingress_ipv6 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='AAAA' AND record->>'value'=host(ingress.public_ingress_ipv6)))))))));
END
$function$;
-- +goose StatementEnd


-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publication_edge_nodes(wanted_kind text, wanted_resource text)
 RETURNS TABLE(node_id text)
 LANGUAGE plpgsql
 STABLE
 SET plan_cache_mode TO 'force_generic_plan'
AS $function$
BEGIN
 -- LIMIT 8 is runtimeattachment.MaximumRegionCarriers: every ready node of the pair's regions, primary region first.
 RETURN QUERY WITH resource AS (
 SELECT account_id,paperboat.publication_workspace_team(wanted_kind,wanted_resource) AS team FROM paperboat.preview_leases WHERE wanted_kind='preview' AND id=wanted_resource
 UNION ALL SELECT account_id,paperboat.publication_workspace_team(wanted_kind,wanted_resource) FROM paperboat.tunnels WHERE wanted_kind='tunnel' AND id=wanted_resource), candidates AS (
 SELECT n.id,coalesce(array_position(s.regions,n.region),9) AS region_rank,EXISTS(SELECT 1 FROM (SELECT expected_records FROM paperboat.preview_domains WHERE wanted_kind='preview' AND preview_id=wanted_resource AND deleted_at IS NULL AND paperboat.workspace_domain_verified(workspace_domain_id) AND conflict_state='clear' UNION ALL SELECT expected_records FROM paperboat.tunnel_domains WHERE wanted_kind='tunnel' AND tunnel_id=wanted_resource AND deleted_at IS NULL AND paperboat.workspace_domain_verified(workspace_domain_id) AND conflict_state='clear') domain CROSS JOIN LATERAL jsonb_array_elements(domain.expected_records) record WHERE record->>'value' IN(host(n.public_ingress_ipv4),host(n.public_ingress_ipv6))) AS retained,row_number() OVER (PARTITION BY n.region ORDER BY n.id) AS serving_rank FROM resource r CROSS JOIN paperboat.control_tunnel_nodes n
 LEFT JOIN paperboat.workspace_edge_sets s ON s.scope_kind=CASE WHEN r.team IS NULL THEN 'account' ELSE 'team' END AND s.scope_id=coalesce(r.team,r.account_id)
 WHERE paperboat.node_workspace_edge_eligible(n.id,r.account_id,r.team)
 AND n.ready AND n.state='ready' AND n.drain_deadline IS NULL AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now()
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.public_ingress_verified_at IS NOT NULL AND(n.public_ingress_ipv4 IS NOT NULL OR n.public_ingress_ipv6 IS NOT NULL)
 AND (NOT EXISTS(SELECT 1 FROM paperboat.selfhost_placements WHERE resource_kind=wanted_kind AND resource_id=wanted_resource)
 OR EXISTS(SELECT 1 FROM paperboat.selfhost_placements p JOIN paperboat.selfhost_installations i ON i.id=p.installation_id WHERE p.resource_kind=wanted_kind AND p.resource_id=wanted_resource AND i.node_id=n.id)))
 -- Managed edges: only the first serving_rank<=4 of a region (edgeset.ServingNodesPerRegion = MaximumRegionCarriers/2;
 -- TestPublicationNodesAreTheServingNodes checks the digit), as edgeset.ServingOnly does for attach. A workspace's own servers
 -- are not limited to the serving set (attach holds carriers to up to MaximumRegionCarriers of them), so all of them stay.
 SELECT id FROM candidates WHERE serving_rank<=4 OR NOT EXISTS(SELECT 1 FROM paperboat.managed_node_authority a WHERE a.node_id=candidates.id) ORDER BY retained DESC,region_rank,id LIMIT 8;
END
$function$;
-- +goose StatementEnd
-- +goose Down
-- Forward only: the previous binding copy is not rebuilt (zero users; reinstall a test database instead).
SELECT 1;
