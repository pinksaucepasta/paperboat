-- +goose Up
CREATE TABLE paperboat.runtime_carrier_attachments (
 route_id text PRIMARY KEY REFERENCES paperboat.control_routes(id) ON DELETE CASCADE,
 machine_id text NOT NULL REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
 account_id text NOT NULL REFERENCES paperboat.users(id),
 installation_generation bigint NOT NULL CHECK (installation_generation > 0),
 worker_generation bigint NOT NULL CHECK (worker_generation > 0),
 process_id text NOT NULL,
 generation bigint NOT NULL CHECK (generation > 0),
 edge_node_id text NOT NULL REFERENCES paperboat.control_tunnel_nodes(id),
 edge_process_epoch text NOT NULL,
 public_identity_key text NOT NULL,
 route_revision bigint NOT NULL CHECK (route_revision > 0),
 session_id text NOT NULL,
 admission jsonb NOT NULL,
 expires_at timestamptz NOT NULL,
 ready boolean NOT NULL DEFAULT false,
 observed_at timestamptz
);
CREATE INDEX runtime_carrier_attachments_edge ON paperboat.runtime_carrier_attachments(edge_node_id);
-- One attachment per existing route bounds storage without retaining payloads or credentials.
-- +goose StatementBegin
CREATE FUNCTION paperboat.runtime_carrier_current(wanted_route text, require_ready boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
 SELECT EXISTS (
 SELECT 1 FROM runtime_carrier_attachments a
 JOIN user_machines m ON m.id=a.machine_id
 JOIN control_routes r ON r.id=a.route_id
 JOIN control_environments e ON e.id=r.environment_id
 JOIN control_tunnel_nodes n ON n.id=a.edge_node_id
 JOIN users u ON u.id=a.account_id
 WHERE a.route_id=wanted_route AND a.expires_at>now()
 AND m.user_id=a.account_id AND u.status='active'
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
