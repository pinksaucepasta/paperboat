-- +goose Up
-- F1 follow-up: a plain drain promises carriers valid until drain_deadline (nodedrain). Self-hosted tunnel routes and
-- non-managed-only previews ask node_route_eligible / node_preview_eligible, which went only through
-- publication_edge_nodes (ready nodes only), so they dropped the moment a drain started. publication_edge_nodes keeps
-- excluding draining nodes (it drives DNS and new placement); eligibility additionally accepts a node that is draining
-- before its deadline and otherwise passes the same per-node checks. Same gate as endpoints/edge.go.

SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE FUNCTION paperboat.publication_edge_node_draining_held(wanted_kind text, wanted_resource text, wanted_node text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM (
 SELECT account_id,paperboat.publication_workspace_team(wanted_kind,wanted_resource) AS team FROM paperboat.preview_leases WHERE wanted_kind='preview' AND id=wanted_resource
 UNION ALL SELECT account_id,paperboat.publication_workspace_team(wanted_kind,wanted_resource) FROM paperboat.tunnels WHERE wanted_kind='tunnel' AND id=wanted_resource) r
 JOIN paperboat.control_tunnel_nodes n ON n.id=wanted_node
 WHERE paperboat.node_workspace_edge_eligible(n.id,r.account_id,r.team)
 AND n.state='draining' AND n.drain_deadline>now() AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now()
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.public_ingress_verified_at IS NOT NULL AND(n.public_ingress_ipv4 IS NOT NULL OR n.public_ingress_ipv6 IS NOT NULL)
 AND (NOT EXISTS(SELECT 1 FROM paperboat.selfhost_placements WHERE resource_kind=wanted_kind AND resource_id=wanted_resource)
 OR EXISTS(SELECT 1 FROM paperboat.selfhost_placements p JOIN paperboat.selfhost_installations i ON i.id=p.installation_id WHERE p.resource_kind=wanted_kind AND p.resource_id=wanted_resource AND i.node_id=n.id)));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.node_preview_eligible(wanted_node text,wanted_account text,wanted_preview text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.preview_leases p JOIN paperboat.control_tunnel_nodes ingress ON ingress.id=wanted_node WHERE p.id=wanted_preview AND p.account_id=wanted_account
 AND paperboat.node_workspace_edge_eligible(wanted_node,wanted_account,p.owner_team_id)
 AND(NOT EXISTS(SELECT 1 FROM paperboat.selfhost_placements WHERE resource_kind='preview' AND resource_id=p.id) OR EXISTS(SELECT 1 FROM paperboat.selfhost_placements placement JOIN paperboat.selfhost_installations i ON i.id=placement.installation_id WHERE placement.resource_kind='preview' AND placement.resource_id=p.id AND i.node_id=wanted_node))
 AND(paperboat.workspace_edge_mode(wanted_account,p.owner_team_id)='managed-only' OR ((wanted_node IN(SELECT node_id FROM paperboat.publication_edge_nodes('preview',p.id)) OR paperboat.publication_edge_node_draining_held('preview',p.id,wanted_node)) AND EXISTS(SELECT 1 FROM paperboat.preview_domains d WHERE d.preview_id=p.id AND d.account_id=wanted_account AND d.deleted_at IS NULL AND paperboat.workspace_domain_verified(d.workspace_domain_id) AND d.conflict_state='clear' AND paperboat.selfhost_publication_hostname_allowed(d.hostname) AND (ingress.public_ingress_ipv4 IS NOT NULL OR ingress.public_ingress_ipv6 IS NOT NULL) AND (ingress.public_ingress_ipv4 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='A' AND record->>'value'=host(ingress.public_ingress_ipv4))) AND (ingress.public_ingress_ipv6 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='AAAA' AND record->>'value'=host(ingress.public_ingress_ipv6)))))));
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
 AND(paperboat.workspace_edge_mode(wanted_account,paperboat.publication_workspace_team('tunnel',t.id))='managed-only' OR ((wanted_node IN(SELECT node_id FROM paperboat.publication_edge_nodes('tunnel',t.id)) OR paperboat.publication_edge_node_draining_held('tunnel',t.id,wanted_node)) AND (route.protocol NOT IN('http','tls') OR EXISTS(SELECT 1 FROM paperboat.tunnel_domains d WHERE d.route_id=route.id AND d.account_id=wanted_account AND d.deleted_at IS NULL AND paperboat.workspace_domain_verified(d.workspace_domain_id) AND d.conflict_state='clear' AND paperboat.selfhost_publication_hostname_allowed(d.hostname) AND (ingress.public_ingress_ipv4 IS NOT NULL OR ingress.public_ingress_ipv6 IS NOT NULL) AND (ingress.public_ingress_ipv4 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='A' AND record->>'value'=host(ingress.public_ingress_ipv4))) AND (ingress.public_ingress_ipv6 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='AAAA' AND record->>'value'=host(ingress.public_ingress_ipv6)))))))));
END
$function$;
-- +goose StatementEnd
-- +goose Down
-- Forward only (zero users; reinstall a test database instead).
SELECT 1;
