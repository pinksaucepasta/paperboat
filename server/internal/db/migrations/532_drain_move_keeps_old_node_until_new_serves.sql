-- +goose Up
-- Drain-and-move (selfhost.repinPlacementsOffNode) repins a self-hosted publication to a sibling server and retargets
-- its domain records there, so the drained node failed both the placement and the expected-records checks of
-- node_route_eligible / node_preview_eligible and cut its routes at once. The repin now records a handoff; the old node
-- stays eligible while it is draining before its deadline (and the handoff's), until the new placement is observed
-- serving there. DNS and new placement (publication_edge_nodes, expected_records) are unchanged: they point at the sibling.
SET LOCAL lock_timeout = '5s';

CREATE TABLE paperboat.selfhost_placement_handoffs(
 resource_kind text NOT NULL CHECK(resource_kind IN ('preview','tunnel')),
 resource_id text NOT NULL,
 from_node_id text NOT NULL,
 until timestamptz NOT NULL,
 PRIMARY KEY(resource_kind,resource_id,from_node_id));

-- +goose StatementBegin
CREATE FUNCTION paperboat.selfhost_handoff_node_held(wanted_kind text,wanted_resource text,wanted_route text,wanted_node text,wanted_account text,wanted_team text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.selfhost_placement_handoffs h JOIN paperboat.control_tunnel_nodes n ON n.id=h.from_node_id
 JOIN paperboat.selfhost_placements placement ON placement.resource_kind=h.resource_kind AND placement.resource_id=h.resource_id
 JOIN paperboat.selfhost_installations target ON target.id=placement.installation_id
 WHERE h.resource_kind=wanted_kind AND h.resource_id=wanted_resource AND h.from_node_id=wanted_node AND h.until>now() AND h.until=n.drain_deadline AND target.node_id<>wanted_node
 AND paperboat.node_workspace_edge_eligible(n.id,wanted_account,wanted_team)
 AND n.state='draining' AND n.drain_deadline>now() AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now()
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.public_ingress_verified_at IS NOT NULL AND(n.public_ingress_ipv4 IS NOT NULL OR n.public_ingress_ipv6 IS NOT NULL)
 -- The new placement serving this route (tunnel) or preview on its node ends the handoff; a handoff belongs to the
 -- drain that recorded it (until = that drain's deadline).
 AND NOT (wanted_kind='tunnel' AND EXISTS(SELECT 1 FROM paperboat.endpoint_edge_assignments x JOIN paperboat.endpoints e ON e.id=x.endpoint_id JOIN paperboat.endpoint_routes er ON er.id=x.route_id AND er.endpoint_id=e.id WHERE e.tunnel_id=wanted_resource AND er.tunnel_route_id=wanted_route AND x.edge_node_id=target.node_id AND x.desired='active' AND x.observed_ready))
 AND NOT (wanted_kind='preview' AND EXISTS(SELECT 1 FROM paperboat.preview_routes a WHERE a.preview_id=wanted_resource AND a.edge_node_id=target.node_id AND a.state='ready' AND a.edge_ready)));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.selfhost_route_handoff_held(wanted_node text,wanted_account text,wanted_route text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.tunnel_routes route JOIN paperboat.tunnels t ON t.id=route.tunnel_id WHERE route.id=wanted_route AND t.account_id=wanted_account
 AND paperboat.selfhost_handoff_node_held('tunnel',t.id,route.id,wanted_node,wanted_account,paperboat.publication_workspace_team('tunnel',t.id))
 -- Same domain checks as node_route_eligible except the address match: the records already point at the new server.
 AND (route.protocol NOT IN('http','tls') OR EXISTS(SELECT 1 FROM paperboat.tunnel_domains d WHERE d.route_id=route.id AND d.account_id=wanted_account AND d.deleted_at IS NULL AND paperboat.workspace_domain_verified(d.workspace_domain_id) AND d.conflict_state='clear' AND paperboat.selfhost_publication_hostname_allowed(d.hostname))));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.selfhost_preview_handoff_held(wanted_node text,wanted_account text,wanted_preview text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.preview_leases p WHERE p.id=wanted_preview AND p.account_id=wanted_account
 AND paperboat.selfhost_handoff_node_held('preview',p.id,NULL,wanted_node,wanted_account,p.owner_team_id)
 AND EXISTS(SELECT 1 FROM paperboat.preview_domains d WHERE d.preview_id=p.id AND d.account_id=wanted_account AND d.deleted_at IS NULL AND paperboat.workspace_domain_verified(d.workspace_domain_id) AND d.conflict_state='clear' AND paperboat.selfhost_publication_hostname_allowed(d.hostname)));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.node_preview_eligible(wanted_node text,wanted_account text,wanted_preview text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.preview_leases p JOIN paperboat.control_tunnel_nodes ingress ON ingress.id=wanted_node WHERE p.id=wanted_preview AND p.account_id=wanted_account
 AND paperboat.node_workspace_edge_eligible(wanted_node,wanted_account,p.owner_team_id)
 AND(NOT EXISTS(SELECT 1 FROM paperboat.selfhost_placements WHERE resource_kind='preview' AND resource_id=p.id) OR EXISTS(SELECT 1 FROM paperboat.selfhost_placements placement JOIN paperboat.selfhost_installations i ON i.id=placement.installation_id WHERE placement.resource_kind='preview' AND placement.resource_id=p.id AND i.node_id=wanted_node))
 AND(paperboat.workspace_edge_mode(wanted_account,p.owner_team_id)='managed-only' OR ((wanted_node IN(SELECT node_id FROM paperboat.publication_edge_nodes('preview',p.id)) OR paperboat.publication_edge_node_draining_held('preview',p.id,wanted_node)) AND EXISTS(SELECT 1 FROM paperboat.preview_domains d WHERE d.preview_id=p.id AND d.account_id=wanted_account AND d.deleted_at IS NULL AND paperboat.workspace_domain_verified(d.workspace_domain_id) AND d.conflict_state='clear' AND paperboat.selfhost_publication_hostname_allowed(d.hostname) AND (ingress.public_ingress_ipv4 IS NOT NULL OR ingress.public_ingress_ipv6 IS NOT NULL) AND (ingress.public_ingress_ipv4 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='A' AND record->>'value'=host(ingress.public_ingress_ipv4))) AND (ingress.public_ingress_ipv6 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='AAAA' AND record->>'value'=host(ingress.public_ingress_ipv6))))))) OR paperboat.selfhost_preview_handoff_held(wanted_node,wanted_account,wanted_preview);
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
 AND(paperboat.workspace_edge_mode(wanted_account,paperboat.publication_workspace_team('tunnel',t.id))='managed-only' OR ((wanted_node IN(SELECT node_id FROM paperboat.publication_edge_nodes('tunnel',t.id)) OR paperboat.publication_edge_node_draining_held('tunnel',t.id,wanted_node)) AND (route.protocol NOT IN('http','tls') OR EXISTS(SELECT 1 FROM paperboat.tunnel_domains d WHERE d.route_id=route.id AND d.account_id=wanted_account AND d.deleted_at IS NULL AND paperboat.workspace_domain_verified(d.workspace_domain_id) AND d.conflict_state='clear' AND paperboat.selfhost_publication_hostname_allowed(d.hostname) AND (ingress.public_ingress_ipv4 IS NOT NULL OR ingress.public_ingress_ipv6 IS NOT NULL) AND (ingress.public_ingress_ipv4 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='A' AND record->>'value'=host(ingress.public_ingress_ipv4))) AND (ingress.public_ingress_ipv6 IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(d.expected_records) record WHERE record->>'type'='AAAA' AND record->>'value'=host(ingress.public_ingress_ipv6))))))))) OR paperboat.selfhost_route_handoff_held(wanted_node,wanted_account,wanted_route);
END
$function$;
-- +goose StatementEnd
-- +goose Down
-- Forward only (zero users; reinstall a test database instead).
SELECT 1;
