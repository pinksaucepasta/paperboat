-- +goose Up
-- Regions. A workspace's pair is two regions (migrations 483-485). This migration makes every caller of
-- node_workspace_edge_eligible pair-aware through one choke point: the old body (membership and the pool/mode rules) is
-- kept as node_workspace_edge_candidate, which only the code that decides the pair may call (edgeset reconcile and
-- selfhost.storePair: they must not depend on the pair they are choosing). Carriers stay valid on a node in Drain until its
-- drain_deadline, so existing connections finish; new attachments are placed only on state='ready' nodes. Publication
-- (preview/tunnel DNS targets) lists every ready node of the pair's regions: the wildcard answers with all of them
-- and edges do not forward. Function replacement only, no data change.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.node_workspace_edge_candidate(wanted_node text, wanted_account text, wanted_team text)
 RETURNS boolean
 LANGUAGE plpgsql
 STABLE
 SET plan_cache_mode TO 'force_generic_plan'
AS $function$
BEGIN
 RETURN (SELECT (wanted_team IS NULL OR EXISTS(SELECT 1 FROM paperboat.team_members member WHERE member.team_id=wanted_team AND member.account_id=wanted_account AND member.active))
 AND paperboat.node_edge_eligible_core(wanted_node,wanted_account,wanted_team));
END
$function$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.node_workspace_edge_eligible(wanted_node text, wanted_account text, wanted_team text)
 RETURNS boolean
 LANGUAGE plpgsql
 STABLE
 SET plan_cache_mode TO 'force_generic_plan'
AS $function$
BEGIN
 RETURN (SELECT paperboat.node_workspace_edge_candidate(wanted_node,wanted_account,wanted_team)
 AND paperboat.workspace_edge_set_allows(wanted_node,wanted_account,wanted_team));
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
 SELECT n.id,coalesce(array_position(s.regions,n.region),9) AS region_rank,EXISTS(SELECT 1 FROM (SELECT expected_records FROM paperboat.preview_domains WHERE wanted_kind='preview' AND preview_id=wanted_resource AND deleted_at IS NULL AND ownership_state='verified' AND conflict_state='clear' UNION ALL SELECT expected_records FROM paperboat.tunnel_domains WHERE wanted_kind='tunnel' AND tunnel_id=wanted_resource AND deleted_at IS NULL AND ownership_state='verified' AND conflict_state='clear') domain CROSS JOIN LATERAL jsonb_array_elements(domain.expected_records) record WHERE record->>'value' IN(host(n.public_ingress_ipv4),host(n.public_ingress_ipv6))) AS retained,row_number() OVER (PARTITION BY n.region ORDER BY n.id) AS serving_rank FROM resource r CROSS JOIN paperboat.control_tunnel_nodes n
 LEFT JOIN paperboat.workspace_edge_sets s ON s.scope_kind=CASE WHEN r.team IS NULL THEN 'account' ELSE 'team' END AND s.scope_id=coalesce(r.team,r.account_id)
 WHERE paperboat.node_workspace_edge_eligible(n.id,r.account_id,r.team)
 AND n.ready AND n.state='ready' AND n.drain_deadline IS NULL AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now()
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.public_ingress_verified_at IS NOT NULL AND(n.public_ingress_ipv4 IS NOT NULL OR n.public_ingress_ipv6 IS NOT NULL)
 AND (NOT EXISTS(SELECT 1 FROM paperboat.selfhost_placements WHERE resource_kind=wanted_kind AND resource_id=wanted_resource)
 OR EXISTS(SELECT 1 FROM paperboat.selfhost_placements p JOIN paperboat.selfhost_installations i ON i.id=p.installation_id WHERE p.resource_kind=wanted_kind AND p.resource_id=wanted_resource AND i.node_id=n.id)))
 -- serving_rank<=4 mirrors edgeset.ServingNodesPerRegion (MaximumRegionCarriers/2); TestPublicationNodesAreTheServingNodes checks the digit.
 SELECT id FROM candidates WHERE serving_rank<=4 ORDER BY retained DESC,region_rank,id LIMIT 8;
END
$function$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.runtime_carrier_valid(wanted_route text, wanted_node text, wanted_epoch text, require_ready boolean)
 RETURNS boolean
 LANGUAGE plpgsql
 STABLE
 SET plan_cache_mode TO 'force_generic_plan'
 SET search_path TO 'paperboat', 'pg_catalog'
AS $function$
BEGIN
 RETURN (SELECT EXISTS (
 SELECT 1 FROM runtime_carrier_attachments a
 JOIN user_machines m ON m.id=a.machine_id
 JOIN control_routes r ON r.id=a.route_id
 JOIN control_environments e ON e.id=r.environment_id
 JOIN control_tunnel_nodes n ON n.id=a.edge_node_id
 JOIN users u ON u.id=a.account_id
 WHERE a.route_id=wanted_route AND a.edge_node_id=wanted_node AND a.edge_process_epoch=wanted_epoch
 AND (a.ready OR a.expires_at>now()) AND m.user_id=a.account_id AND u.status='active'
 AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.seat_state='occupied'
 AND m.installation_generation=a.installation_generation AND m.worker_generation>=a.worker_generation
 AND m.public_identity_key=a.public_identity_key
 AND r.environment_id=m.environment_id AND r.desired_revision=a.route_revision
 AND r.kind='runtime_https_wss' AND r.desired_state IN ('attached','replacing') AND e.desired_state='active'
 AND ((n.ready AND n.state='ready') OR (n.state='draining' AND n.drain_deadline>now())) AND n.process_epoch=a.edge_process_epoch
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.last_heartbeat_at>now()-interval '30 seconds' AND n.registry_expires_at>now()
 AND (n.drain_deadline IS NULL OR n.drain_deadline>now())
 AND (paperboat.node_edge_eligible_core(n.id,a.account_id,m.owner_team_id) OR paperboat.node_managed_edge_eligible(n.id,a.account_id,m.owner_team_id))
 AND r.public_host=r.managed_public_host
 AND (NOT require_ready OR (a.ready AND a.observed_at>now()-interval '2 minutes'))
 ));
END
$function$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.runtime_carrier_current(wanted_route text, require_ready boolean)
 RETURNS boolean
 LANGUAGE sql
 STABLE
 SET search_path TO 'paperboat', 'pg_catalog'
AS $function$
 SELECT EXISTS (
 SELECT 1 FROM runtime_carrier_attachments a
 JOIN user_machines m ON m.id=a.machine_id
 JOIN control_routes r ON r.id=a.route_id
 JOIN control_environments e ON e.id=r.environment_id
 JOIN control_tunnel_nodes n ON n.id=a.edge_node_id
 JOIN users u ON u.id=a.account_id
 WHERE a.route_id=wanted_route AND (a.ready OR a.expires_at>now())
 AND m.user_id=a.account_id AND u.status='active'
 AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.seat_state='occupied'
 AND m.installation_generation=a.installation_generation AND m.worker_generation>=a.worker_generation
 AND m.public_identity_key=a.public_identity_key
 AND r.environment_id=m.environment_id AND r.desired_revision=a.route_revision
 AND r.kind='runtime_https_wss' AND r.desired_state IN ('attached','replacing') AND e.desired_state='active'
 AND ((n.ready AND n.state='ready') OR (n.state='draining' AND n.drain_deadline>now())) AND n.process_epoch=a.edge_process_epoch
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.last_heartbeat_at>now()-interval '30 seconds' AND n.registry_expires_at>now()
 AND (n.drain_deadline IS NULL OR n.drain_deadline>now())
 AND (paperboat.node_edge_eligible_core(n.id,a.account_id,m.owner_team_id) OR paperboat.node_managed_edge_eligible(n.id,a.account_id,m.owner_team_id))
 AND r.public_host=r.managed_public_host
 AND (NOT require_ready OR (a.ready AND a.observed_at>now()-interval '2 minutes'))
 );
$function$;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
