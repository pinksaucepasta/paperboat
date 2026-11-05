-- +goose Up
-- A worker handover registers the next worker generation before its attach replaces the old carrier. 229 kept
-- that carrier valid (runtime_carrier_valid) through the overlap; runtime_carrier_current, which every helper
-- route lookup reads, still required the exact generation, so the machine had no route until the attach.
SET LOCAL lock_timeout = '5s';
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
 AND n.ready AND n.state='ready' AND n.process_epoch=a.edge_process_epoch
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.last_heartbeat_at>now()-interval '30 seconds' AND n.registry_expires_at>now()
 AND (n.drain_deadline IS NULL OR n.drain_deadline>now())
 AND paperboat.node_edge_eligible_core(n.id,a.account_id,m.owner_team_id)
 AND (CASE WHEN paperboat.workspace_edge_mode(a.account_id,m.owner_team_id)='managed-only' THEN r.public_host=r.managed_public_host ELSE r.public_host<>r.managed_public_host END)
 AND (NOT require_ready OR (a.ready AND a.observed_at>now()-interval '2 minutes'))
 );
$function$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
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
  AND m.installation_generation=a.installation_generation AND m.worker_generation=a.worker_generation
 AND m.public_identity_key=a.public_identity_key
 AND r.environment_id=m.environment_id AND r.desired_revision=a.route_revision
 AND r.kind='runtime_https_wss' AND r.desired_state IN ('attached','replacing') AND e.desired_state='active'
 AND n.ready AND n.state='ready' AND n.process_epoch=a.edge_process_epoch
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.last_heartbeat_at>now()-interval '30 seconds' AND n.registry_expires_at>now()
 AND (n.drain_deadline IS NULL OR n.drain_deadline>now())
 AND paperboat.node_edge_eligible_core(n.id,a.account_id,m.owner_team_id)
 AND (CASE WHEN paperboat.workspace_edge_mode(a.account_id,m.owner_team_id)='managed-only' THEN r.public_host=r.managed_public_host ELSE r.public_host<>r.managed_public_host END)
 AND (NOT require_ready OR (a.ready AND a.observed_at>now()-interval '2 minutes'))
 );
$function$;
-- +goose StatementEnd
