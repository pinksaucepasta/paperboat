-- +goose Up
-- Managed runtime hostnames use only platform-certificate edges. Pool policy
-- still applies: self-hosted-only never silently falls back to a managed edge.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.runtime_carrier_valid(wanted_route text,wanted_node text,wanted_epoch text,require_ready boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
 SELECT EXISTS (
 SELECT 1 FROM runtime_carrier_attachments a
 JOIN user_machines m ON m.id=a.machine_id
 JOIN control_routes r ON r.id=a.route_id
 JOIN control_environments e ON e.id=r.environment_id
 JOIN control_tunnel_nodes n ON n.id=a.edge_node_id
 JOIN users u ON u.id=a.account_id
 WHERE a.route_id=wanted_route AND a.edge_node_id=wanted_node AND a.edge_process_epoch=wanted_epoch
 AND a.expires_at>now() AND m.user_id=a.account_id AND u.status='active'
 AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.setup_mode='host'
 AND m.installation_generation=a.installation_generation AND m.worker_generation=a.worker_generation
 AND m.public_identity_key=a.public_identity_key
 AND r.environment_id=m.environment_id AND r.desired_revision=a.route_revision
 AND r.kind='runtime_https_wss' AND r.desired_state IN ('attached','replacing') AND e.desired_state='active'
 AND n.ready AND n.state='ready' AND n.process_epoch=a.edge_process_epoch
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.last_heartbeat_at>now()-interval '30 seconds' AND n.registry_expires_at>now()
 AND (n.drain_deadline IS NULL OR n.drain_deadline>now())
 AND NOT EXISTS (SELECT 1 FROM selfhost_installations i WHERE i.node_id=n.id)
 AND paperboat.node_pool_eligible(n.id,a.account_id,'tunnel')
 AND (NOT require_ready OR (a.ready AND a.observed_at>now()-interval '30 seconds'))
 );
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.runtime_carrier_valid(wanted_route text,wanted_node text,wanted_epoch text,require_ready boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
 SELECT EXISTS (
 SELECT 1 FROM runtime_carrier_attachments a
 JOIN user_machines m ON m.id=a.machine_id
 JOIN control_routes r ON r.id=a.route_id
 JOIN control_environments e ON e.id=r.environment_id
 JOIN control_tunnel_nodes n ON n.id=a.edge_node_id
 JOIN users u ON u.id=a.account_id
 WHERE a.route_id=wanted_route AND a.edge_node_id=wanted_node AND a.edge_process_epoch=wanted_epoch
 AND a.expires_at>now() AND m.user_id=a.account_id AND u.status='active'
 AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.setup_mode='host'
 AND m.installation_generation=a.installation_generation AND m.worker_generation=a.worker_generation
 AND m.public_identity_key=a.public_identity_key
 AND r.environment_id=m.environment_id AND r.desired_revision=a.route_revision
 AND r.kind='runtime_https_wss' AND r.desired_state IN ('attached','replacing') AND e.desired_state='active'
 AND n.ready AND n.state='ready' AND n.process_epoch=a.edge_process_epoch
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.last_heartbeat_at>now()-interval '30 seconds' AND n.registry_expires_at>now()
 AND (n.drain_deadline IS NULL OR n.drain_deadline>now())
 AND paperboat.node_pool_eligible(n.id,a.account_id,'tunnel')
 AND (NOT require_ready OR (a.ready AND a.observed_at>now()-interval '30 seconds'))
 );
$$;
-- +goose StatementEnd
