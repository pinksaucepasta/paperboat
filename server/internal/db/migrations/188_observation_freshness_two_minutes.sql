-- +goose Up
-- Edges repeat their carrier and endpoint observations every few seconds; an unchanged one used to refresh
-- observed_at every 10 s because readiness required an observation newer than 30 s, ~1,270 row writes an hour
-- per idle machine (PLAN 3b, F21-19). A carrier or route going not-ready is still written at once; a dead edge is
-- caught by its node heartbeat (30 s) and an edge restart by its process epoch, both checked next to observed_at
-- in every reader below. So readiness accepts an observation up to 2 minutes old, and edges refresh an unchanged
-- one every 60 s. The endpoint branch of preview_routes now checks the edge's heartbeat and epoch too.
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.runtime_carrier_valid(wanted_route text, wanted_node text, wanted_epoch text, require_ready boolean)
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
 WHERE a.route_id=wanted_route AND a.edge_node_id=wanted_node AND a.edge_process_epoch=wanted_epoch
 AND (a.ready OR a.expires_at>now()) AND m.user_id=a.account_id AND u.status='active'
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
-- +goose StatementBegin
CREATE OR REPLACE VIEW paperboat.preview_routes AS
SELECT a.preview_id, a.account_id, a.operation_id, a.owner_machine_id, a.owner_session_id, a.host_id,
 a.tunnel_id, a.connector_id, a.connector_session_id, a.process_generation, a.config_generation,
 a.route_id, a.route_generation, a.lease_generation, a.config_content_hash, a.edge_node_id,
 a.edge_process_epoch, a.attachment_generation, a.machine_identity_public_key, a.edge_endpoints,
 a.edge_carrier_server_spki_sha256, a.edge_carrier_server_certificate_chain_pem, a.state, a.edge_ready,
 a.origin_ready, a.expires_at, false AS endpoint
FROM paperboat.preview_lease_carrier_attachments a
UNION ALL
SELECT p.id, p.account_id, x.route_id, p.owner_machine_id, p.owner_session_id, p.owner_machine_id,
 ra.admission->'binding'->>'tunnel_id', ra.admission->'binding'->>'connector_id', ra.session_id,
 ra.worker_generation, (ra.admission->'binding'->>'config_generation')::bigint,
 x.route_id, r.revision, 1::bigint, ra.admission->>'config_content_hash', x.edge_node_id,
 x.edge_process_epoch, x.generation, ra.public_identity_key,
 ARRAY(SELECT jsonb_array_elements_text(ra.admission->'edge_endpoints')),
 ra.admission->'binding'->>'edge_carrier_server_spki_sha256',
 ra.admission->'binding'->>'edge_carrier_server_certificate_chain_pem',
 CASE WHEN p.ready_at IS NOT NULL THEN 'ready' ELSE 'edge_ready' END, true, p.ready_at IS NOT NULL,
 ra.expires_at, true
FROM paperboat.endpoint_edge_assignments x
JOIN paperboat.endpoint_routes r ON r.id=x.route_id
JOIN paperboat.endpoints e ON e.id=x.endpoint_id AND e.kind='preview' AND e.desired_state='active'
JOIN paperboat.preview_leases p ON p.id=e.preview_lease_id
JOIN paperboat.runtime_carrier_attachments ra ON ra.machine_id=e.agent_machine_id AND ra.edge_node_id=x.edge_node_id
 AND ra.edge_process_epoch=x.edge_process_epoch AND ra.session_id=x.carrier_session_id
JOIN paperboat.control_tunnel_nodes n ON n.id=x.edge_node_id AND n.process_epoch=x.edge_process_epoch
 AND n.last_heartbeat_at>now()-interval '30 seconds'
WHERE x.desired='active' AND x.observed_ready AND x.observed_at>now()-interval '2 minutes'
 AND ra.expires_at>now()
 AND EXISTS(SELECT 1 FROM paperboat.user_machines m WHERE m.id=e.agent_machine_id AND m.revoked_at IS NULL AND m.deleted_at IS NULL);
-- +goose StatementEnd

-- +goose Down
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
 AND (NOT require_ready OR (a.ready AND a.observed_at>now()-interval '30 seconds'))
 );
$function$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.runtime_carrier_valid(wanted_route text, wanted_node text, wanted_epoch text, require_ready boolean)
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
 WHERE a.route_id=wanted_route AND a.edge_node_id=wanted_node AND a.edge_process_epoch=wanted_epoch
 AND (a.ready OR a.expires_at>now()) AND m.user_id=a.account_id AND u.status='active'
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
 AND (NOT require_ready OR (a.ready AND a.observed_at>now()-interval '30 seconds'))
 );
$function$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE VIEW paperboat.preview_routes AS
SELECT a.preview_id, a.account_id, a.operation_id, a.owner_machine_id, a.owner_session_id, a.host_id,
 a.tunnel_id, a.connector_id, a.connector_session_id, a.process_generation, a.config_generation,
 a.route_id, a.route_generation, a.lease_generation, a.config_content_hash, a.edge_node_id,
 a.edge_process_epoch, a.attachment_generation, a.machine_identity_public_key, a.edge_endpoints,
 a.edge_carrier_server_spki_sha256, a.edge_carrier_server_certificate_chain_pem, a.state, a.edge_ready,
 a.origin_ready, a.expires_at, false AS endpoint
FROM paperboat.preview_lease_carrier_attachments a
UNION ALL
SELECT p.id, p.account_id, x.route_id, p.owner_machine_id, p.owner_session_id, p.owner_machine_id,
 ra.admission->'binding'->>'tunnel_id', ra.admission->'binding'->>'connector_id', ra.session_id,
 ra.worker_generation, (ra.admission->'binding'->>'config_generation')::bigint,
 x.route_id, r.revision, 1::bigint, ra.admission->>'config_content_hash', x.edge_node_id,
 x.edge_process_epoch, x.generation, ra.public_identity_key,
 ARRAY(SELECT jsonb_array_elements_text(ra.admission->'edge_endpoints')),
 ra.admission->'binding'->>'edge_carrier_server_spki_sha256',
 ra.admission->'binding'->>'edge_carrier_server_certificate_chain_pem',
 CASE WHEN p.ready_at IS NOT NULL THEN 'ready' ELSE 'edge_ready' END, true, p.ready_at IS NOT NULL,
 ra.expires_at, true
FROM paperboat.endpoint_edge_assignments x
JOIN paperboat.endpoint_routes r ON r.id=x.route_id
JOIN paperboat.endpoints e ON e.id=x.endpoint_id AND e.kind='preview' AND e.desired_state='active'
JOIN paperboat.preview_leases p ON p.id=e.preview_lease_id
JOIN paperboat.runtime_carrier_attachments ra ON ra.machine_id=e.agent_machine_id AND ra.edge_node_id=x.edge_node_id
 AND ra.edge_process_epoch=x.edge_process_epoch AND ra.session_id=x.carrier_session_id
WHERE x.desired='active' AND x.observed_ready AND x.observed_at>now()-interval '30 seconds'
 AND ra.expires_at>now()
 AND EXISTS(SELECT 1 FROM paperboat.user_machines m WHERE m.id=e.agent_machine_id AND m.revoked_at IS NULL AND m.deleted_at IS NULL);
-- +goose StatementEnd
