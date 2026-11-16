-- +goose Up
-- One definition of "an endpoint route is ready", for previews and tunnels alike: every live edge holding the
-- route's active assignment on the agent's runtime carrier has observed it ready. Both edges of an account answer
-- the same wildcard name and do not forward to each other, so "ready on any edge" sent visitors to an edge
-- without the route. A draining, unregistered, stale or ineligible edge is not waited for. This is the probe
-- tunnelv1.RefreshHealth inlined; the preview readiness gate (endpoints.PreviewReadiness) used ANY edge
-- through preview_routes. PL/pgSQL with force_generic_plan keeps the plan for the session (migrations 296, 372).
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.endpoint_route_ready(wanted_route text, at timestamptz)
 RETURNS boolean
 LANGUAGE plpgsql
 STABLE
 SET plan_cache_mode TO 'force_generic_plan'
AS $function$
BEGIN
 RETURN (SELECT coalesce(bool_and(x.observed_ready AND x.observed_at>at-interval '2 minutes'),false)
 FROM paperboat.endpoint_edge_assignments x
 JOIN paperboat.endpoints e ON e.id=x.endpoint_id AND e.desired_state='active'
 JOIN paperboat.endpoint_routes er ON er.id=x.route_id
 JOIN paperboat.runtime_carrier_attachments ra ON ra.machine_id=e.agent_machine_id AND ra.edge_node_id=x.edge_node_id
  AND ra.edge_process_epoch=x.edge_process_epoch AND ra.session_id=x.carrier_session_id
 JOIN paperboat.control_tunnel_nodes n ON n.id=x.edge_node_id AND n.process_epoch=x.edge_process_epoch
 WHERE x.route_id=wanted_route AND x.desired='active' AND ra.expires_at>at
  AND n.state='ready' AND n.ready AND n.roles @> ARRAY['edge']::text[]
  AND n.registry_expires_at>at AND n.last_heartbeat_at>at-interval '15 seconds' AND n.drain_deadline IS NULL
  AND CASE e.kind WHEN 'tunnel' THEN paperboat.node_route_eligible(n.id,e.account_id,er.tunnel_route_id)
   ELSE paperboat.node_preview_eligible(n.id,e.account_id,e.preview_lease_id) END);
END
$function$;
-- +goose StatementEnd

-- The managed-preview pending flag (preview lists, decoratePreviewEndpoints) reads the same rule instead of its
-- own edge scan: a managed preview is pending until it is served with its origin ready and the route is ready on
-- every live, eligible edge holding it.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.preview_managed_route_pending(wanted_preview text)
 RETURNS boolean
 LANGUAGE plpgsql
 STABLE
 SET plan_cache_mode TO 'force_generic_plan'
AS $function$
DECLARE
 lease_account text;
 lease_team text;
BEGIN
 SELECT account_id,owner_team_id INTO lease_account,lease_team FROM paperboat.preview_leases WHERE id=wanted_preview;
 IF NOT FOUND THEN
  RETURN false;
 END IF;
 RETURN paperboat.workspace_edge_mode(lease_account,lease_team)='managed-only' AND (
 NOT EXISTS (SELECT 1 FROM paperboat.preview_routes a WHERE a.preview_id=wanted_preview AND a.account_id=lease_account
  AND a.state='ready' AND a.edge_ready AND a.origin_ready AND a.expires_at>now())
 OR NOT EXISTS (SELECT 1 FROM paperboat.endpoints e JOIN paperboat.endpoint_routes er ON er.endpoint_id=e.id
  WHERE e.preview_lease_id=wanted_preview AND paperboat.endpoint_route_ready(er.id,now())));
END
$function$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS paperboat.endpoint_route_ready(text, timestamptz);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.preview_managed_route_pending(wanted_preview text)
 RETURNS boolean
 LANGUAGE plpgsql
 STABLE
 SET plan_cache_mode TO 'force_generic_plan'
AS $function$
DECLARE
 lease_account text;
 lease_team text;
BEGIN
 SELECT account_id,owner_team_id INTO lease_account,lease_team FROM paperboat.preview_leases WHERE id=wanted_preview;
 IF NOT FOUND THEN
  RETURN false;
 END IF;
 RETURN paperboat.workspace_edge_mode(lease_account,lease_team)='managed-only' AND (
 NOT EXISTS (SELECT 1 FROM paperboat.preview_routes a WHERE a.preview_id=wanted_preview AND a.account_id=lease_account
  AND a.state='ready' AND a.edge_ready AND a.origin_ready AND a.expires_at>now())
 OR EXISTS (SELECT 1 FROM paperboat.control_tunnel_nodes n
  WHERE n.state='ready' AND n.ready AND n.drain_deadline IS NULL AND n.roles @> ARRAY['edge']::text[]
  AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now() AND n.public_ingress_verified_at IS NOT NULL
  AND paperboat.node_workspace_edge_eligible(n.id,lease_account,lease_team)
  AND NOT EXISTS (SELECT 1 FROM paperboat.preview_routes a WHERE a.preview_id=wanted_preview AND a.account_id=lease_account
   AND a.edge_node_id=n.id AND a.edge_process_epoch=n.process_epoch
   AND a.state='ready' AND a.edge_ready AND a.origin_ready AND a.expires_at>now())));
END
$function$;
-- +goose StatementEnd
