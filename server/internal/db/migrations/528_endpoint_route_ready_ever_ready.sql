-- +goose Up
-- Every-holder readiness (526) drops an edge from the wait only when the machine's carrier there has never been
-- ready since it was assigned (ever_ready, reset with attached_at) and its 45 s attach window has passed. An edge
-- that was ready and then lost its carrier (a flap, a reconnect on the same admission, which keeps attached_at)
-- is still waited for: excluding it made a preview ready, settled an address move and marked certificates and
-- health served while that edge answered 404/502. The window is measured on the database clock, the clock that
-- writes attached_at, not the caller's `at` (an app clock skewed by 45 s collapsed it).
-- 529 backfills ever_ready for carriers that are ready now. Deploy: run 528-529 before starting servers that write ever_ready (the usual migrate-then-roll order).
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.runtime_carrier_attachments ADD COLUMN ever_ready boolean NOT NULL DEFAULT false;
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
 WHERE x.route_id=wanted_route AND x.desired='active' AND ra.expires_at>at AND (NOT every_holder OR ra.ready OR ra.ever_ready OR ra.attached_at>now()-interval '45 seconds')
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
 WHERE x.route_id=wanted_route AND x.desired='active' AND ra.expires_at>at AND (NOT every_holder OR ra.ready OR ra.attached_at>at-interval '45 seconds')
  AND n.state='ready' AND n.ready AND n.roles @> ARRAY['edge']::text[]
  AND n.registry_expires_at>at AND n.last_heartbeat_at>at-interval '15 seconds' AND n.drain_deadline IS NULL
  AND CASE e.kind WHEN 'tunnel' THEN paperboat.node_route_eligible(n.id,e.account_id,er.tunnel_route_id)
   ELSE paperboat.node_preview_eligible(n.id,e.account_id,e.preview_lease_id) END);
END
$function$;
-- +goose StatementEnd

ALTER TABLE paperboat.runtime_carrier_attachments DROP COLUMN ever_ready;
