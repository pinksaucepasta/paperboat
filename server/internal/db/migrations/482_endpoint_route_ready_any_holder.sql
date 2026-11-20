-- +goose Up
-- A route is ready for its owner when at least one live edge holding its active assignment on the agent's runtime
-- carrier has observed it ready, not when every one has (377): one live edge that lags (a missed observation
-- refresh, a deploy, a slow pull) used to withdraw the route from the edges serving it, so every route that edge
-- held flapped. The lagging edge alone answers visitors wrongly until it converges; no live holder is still not
-- ready. every_holder=true keeps 377's rule for the address move, which may only drop the old name once every
-- edge serves the new one. Preview readiness, tunnel health and the managed-preview pending flag pass false.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.endpoint_route_ready(wanted_route text, at timestamptz, every_holder boolean)
 RETURNS boolean
 LANGUAGE plpgsql
 STABLE
 SET plan_cache_mode TO 'force_generic_plan'
AS $function$
BEGIN
 RETURN (SELECT coalesce(CASE WHEN every_holder THEN bool_and(x.observed_ready AND x.observed_at>at-interval '2 minutes')
  ELSE bool_or(x.observed_ready AND x.observed_at>at-interval '2 minutes') END,false)
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
  WHERE e.preview_lease_id=wanted_preview AND paperboat.endpoint_route_ready(er.id,now(),false)));
END
$function$;
-- +goose StatementEnd

-- +goose StatementBegin
DROP FUNCTION IF EXISTS paperboat.endpoint_route_ready(text, timestamptz);
-- +goose StatementEnd

-- +goose Down
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
-- +goose StatementBegin
DROP FUNCTION IF EXISTS paperboat.endpoint_route_ready(text, timestamptz, boolean);
-- +goose StatementEnd
