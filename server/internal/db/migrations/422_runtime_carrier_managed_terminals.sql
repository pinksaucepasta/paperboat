-- +goose Up
-- Browser terminals (and the runtime route generally: machine access, file inbox, preview dispatch) always go through
-- Paperboat's managed edges at its own domain, whatever the workspace edge mode; a customer-run edge never carries
-- them. So a runtime carrier is valid on a managed edge in every mode, and on a workspace's own edge only
-- for the tunnels and previews riding it; the route's public host is always the managed one.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.node_managed_edge_eligible(wanted_node text, wanted_account text, wanted_team text)
 RETURNS boolean
 LANGUAGE plpgsql
 STABLE
 SET plan_cache_mode TO 'force_generic_plan'
AS $function$
BEGIN
 RETURN (SELECT EXISTS(SELECT 1 FROM paperboat.control_tunnel_nodes n
 LEFT JOIN paperboat.managed_node_authority m ON m.node_id=n.id
 LEFT JOIN paperboat.operator_regions region ON region.id=m.region_id
 WHERE n.id=wanted_node AND (wanted_team IS NULL OR EXISTS(SELECT 1 FROM paperboat.teams t WHERE t.team_id=wanted_team AND t.deleted_at IS NULL AND NOT t.suspended))
 AND NOT EXISTS(SELECT 1 FROM paperboat.selfhost_installations i WHERE i.node_id=n.id AND i.scope_kind<>'global')
 AND (m.node_id IS NULL OR(m.retired_at IS NULL AND region.enabled))
 AND(n.allowed_account_ids IS NULL OR wanted_account=ANY(n.allowed_account_ids))));
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
 AND n.ready AND n.state='ready' AND n.process_epoch=a.edge_process_epoch
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
 AND n.ready AND n.state='ready' AND n.process_epoch=a.edge_process_epoch
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
SET LOCAL lock_timeout = '5s';
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
 AND n.ready AND n.state='ready' AND n.process_epoch=a.edge_process_epoch
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.last_heartbeat_at>now()-interval '30 seconds' AND n.registry_expires_at>now()
 AND (n.drain_deadline IS NULL OR n.drain_deadline>now())
 AND paperboat.node_edge_eligible_core(n.id,a.account_id,m.owner_team_id)
 AND (CASE WHEN paperboat.workspace_edge_mode(a.account_id,m.owner_team_id)='managed-only' THEN r.public_host=r.managed_public_host ELSE r.public_host<>r.managed_public_host END)
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

DROP FUNCTION paperboat.node_managed_edge_eligible(text, text, text);
