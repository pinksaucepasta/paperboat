-- +goose Up
-- Every-holder readiness (preview gate and managed pending flag, 525; address move) counts only edges on which
-- the machine's carrier is up or still attaching (ra.ready, set by that edge's own report). An assignment is made for every carrier
-- row, including one to an edge the machine never managed to reach (firewalled, attach failing), and that edge
-- could never observe the route, so the preview never became ready (F2, support_f5af40d7). An edge whose carrier
-- is up still has to serve the route (525's guarantee); with no carrier up anywhere it stays not ready.
-- An edge whose carrier is only connecting (first attach, worker handover, edge restart: attached_at is reset
-- whenever a new carrier is assigned there) is still waited for during a bounded attach window, 45 s (the agent's
-- connect takes up to 12 s per try with backoff), so a slow second edge does not get a 404/502 or lose the old
-- address of a move. Any-holder mode is unchanged: observed_ready already requires a ready carrier.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.runtime_carrier_attachments ADD COLUMN attached_at timestamptz NOT NULL DEFAULT now();
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
 WHERE x.route_id=wanted_route AND x.desired='active' AND ra.expires_at>at AND (NOT every_holder OR ra.ready OR ra.attached_at>at-interval '45 seconds')
  AND n.state='ready' AND n.ready AND n.roles @> ARRAY['edge']::text[]
  AND n.registry_expires_at>at AND n.last_heartbeat_at>at-interval '15 seconds' AND n.drain_deadline IS NULL
  AND CASE e.kind WHEN 'tunnel' THEN paperboat.node_route_eligible(n.id,e.account_id,er.tunnel_route_id)
   ELSE paperboat.node_preview_eligible(n.id,e.account_id,e.preview_lease_id) END);
END
$function$;
-- +goose StatementEnd

-- +goose Down
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
ALTER TABLE paperboat.runtime_carrier_attachments DROP COLUMN attached_at;
