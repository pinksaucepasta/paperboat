-- +goose Up
-- A device reports its preferred edge pair after probing server-authorized
-- candidates. The control plane still checks live readiness and resource
-- eligibility before assigning any route to one of these nodes.
CREATE TABLE paperboat.device_edge_placements (
    machine_id text PRIMARY KEY REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
    account_id text NOT NULL REFERENCES paperboat.users(id),
    installation_generation bigint NOT NULL CHECK (installation_generation > 0),
    worker_generation bigint NOT NULL CHECK (worker_generation > 0),
    primary_node_id text NOT NULL,
    primary_process_epoch text NOT NULL,
    secondary_node_id text,
    secondary_process_epoch text,
    observed_at timestamptz NOT NULL,
    CHECK ((secondary_node_id IS NULL) = (secondary_process_epoch IS NULL)),
    CHECK (secondary_node_id IS NULL OR secondary_node_id <> primary_node_id)
);
CREATE INDEX device_edge_placements_account ON paperboat.device_edge_placements(account_id);

-- A runtime route may be prepared on two independent edges. Only its active
-- row can back browser tickets; both rows retain exact edge/process fencing.
ALTER TABLE paperboat.runtime_carrier_attachments DROP CONSTRAINT runtime_carrier_attachments_pkey;
ALTER TABLE paperboat.runtime_carrier_attachments ADD COLUMN active boolean NOT NULL DEFAULT false;
UPDATE paperboat.runtime_carrier_attachments SET active=true;
ALTER TABLE paperboat.runtime_carrier_attachments ADD PRIMARY KEY(route_id,edge_node_id);
CREATE UNIQUE INDEX runtime_carrier_one_active ON paperboat.runtime_carrier_attachments(route_id) WHERE active;

-- +goose StatementBegin
CREATE FUNCTION paperboat.runtime_carrier_valid(wanted_route text,wanted_node text,wanted_epoch text,require_ready boolean) RETURNS boolean
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.runtime_carrier_current(wanted_route text,require_ready boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
 SELECT EXISTS (
 SELECT 1 FROM runtime_carrier_attachments a
 WHERE a.route_id=wanted_route AND a.active
 AND paperboat.runtime_carrier_valid(a.route_id,a.edge_node_id,a.edge_process_epoch,require_ready)
 );
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.runtime_carrier_current(wanted_route text,require_ready boolean) RETURNS boolean
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
DROP FUNCTION paperboat.runtime_carrier_valid(text,text,text,boolean);
DROP INDEX paperboat.runtime_carrier_one_active;
ALTER TABLE paperboat.runtime_carrier_attachments DROP CONSTRAINT runtime_carrier_attachments_pkey;
DELETE FROM paperboat.runtime_carrier_attachments WHERE NOT active;
ALTER TABLE paperboat.runtime_carrier_attachments DROP COLUMN active;
ALTER TABLE paperboat.runtime_carrier_attachments ADD PRIMARY KEY(route_id);
DROP TABLE paperboat.device_edge_placements;
