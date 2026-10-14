-- +goose Up
-- Product telemetry is separate from quota debits. Accepted edge receipts remain
-- the source of byte deltas; snapshot labels here so deletion cannot rewrite history.
CREATE TABLE paperboat.resource_bandwidth_events (
 operation_id text PRIMARY KEY,
 workspace_id text NOT NULL,
 resource_kind text NOT NULL,
 resource_id text NOT NULL,
 resource_name text NOT NULL,
 resource_route_id text NOT NULL DEFAULT '',
 machine_id text NOT NULL DEFAULT '',
 machine_name text NOT NULL DEFAULT '',
 mode text NOT NULL CHECK(mode IN ('direct','relay','edge','unknown')),
 hosting text NOT NULL CHECK(hosting IN ('managed','self_hosted','none','unknown')),
 node_id text NOT NULL DEFAULT '',
 node_name text NOT NULL DEFAULT '',
 upload_bytes bigint NOT NULL CHECK(upload_bytes>=0),
 download_bytes bigint NOT NULL CHECK(download_bytes>=0),
 interval_start timestamptz NOT NULL,
 interval_end timestamptz NOT NULL CHECK(interval_end>=interval_start),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX resource_bandwidth_retention ON paperboat.resource_bandwidth_events(interval_end);
CREATE INDEX resource_bandwidth_machine_minute ON paperboat.resource_bandwidth_events(machine_id,interval_start);
CREATE INDEX resource_bandwidth_workspace_time ON paperboat.resource_bandwidth_events(workspace_id,interval_end);
-- +goose StatementBegin
CREATE FUNCTION paperboat.project_edge_bandwidth(receipt_operation text) RETURNS void LANGUAGE sql AS $$
 DELETE FROM paperboat.resource_bandwidth_events WHERE operation_id IN (SELECT operation_id FROM paperboat.resource_bandwidth_events WHERE interval_end<now()-interval '90 days' LIMIT 500);
 INSERT INTO paperboat.resource_bandwidth_events(operation_id,workspace_id,resource_kind,resource_id,resource_name,machine_id,machine_name,mode,hosting,node_id,node_name,upload_bytes,download_bytes,interval_start,interval_end)
 SELECT 'edge:'||r.operation_id,
 coalesce(r.workspace_id,a.workspace_id,CASE WHEN p.id IS NOT NULL THEN coalesce('team:'||p.owner_team_id,'personal:'||p.account_id) WHEN t.id IS NOT NULL THEN coalesce('team:'||t.owner_team_id,'personal:'||t.account_id) END),
 CASE WHEN p.id IS NOT NULL THEN 'preview' WHEN t.id IS NOT NULL THEN 'tunnel' ELSE 'route' END,
 coalesce(p.id,t.id,r.route_id),coalesce(p.endpoint,t.name,r.route_id),coalesce(m.id,''),coalesce(m.alias,''),
 'edge',CASE WHEN i.scope_kind IN ('account','team') THEN 'self_hosted' WHEN n.id IS NOT NULL THEN 'managed' ELSE 'unknown' END,
 r.edge_node_id,coalesce(i.name,r.edge_node_id),CASE WHEN r.direction='ingress' THEN r.delta_bytes ELSE 0 END,CASE WHEN r.direction='egress' THEN r.delta_bytes ELSE 0 END,r.interval_start,r.interval_end
 FROM paperboat.control_usage_receipts r
 LEFT JOIN paperboat.usage_accounting_authorities a ON a.authority_id=r.authority_id AND a.edge_node_id=r.edge_node_id AND a.environment_id=r.environment_id AND a.route_id=r.route_id AND a.route_revision=r.route_revision
 LEFT JOIN LATERAL (SELECT preview_id FROM paperboat.preview_lease_carrier_attachments WHERE route_id=r.route_id AND edge_node_id=r.edge_node_id AND route_generation=r.route_revision ORDER BY created_at DESC LIMIT 1) pa ON true
 LEFT JOIN paperboat.preview_leases p ON p.id=pa.preview_id
 LEFT JOIN paperboat.tunnel_routes tr ON tr.id=r.route_id
 LEFT JOIN paperboat.tunnels t ON t.id=tr.tunnel_id AND p.id IS NULL
 LEFT JOIN paperboat.user_machines m ON m.id=coalesce(p.owner_machine_id,t.created_by_host_id)
 LEFT JOIN paperboat.selfhost_installations i ON i.node_id=r.edge_node_id
 LEFT JOIN paperboat.control_tunnel_nodes n ON n.id=r.edge_node_id
 WHERE r.operation_id=receipt_operation AND r.delta_bytes>0
 AND coalesce(r.workspace_id,a.workspace_id,CASE WHEN p.id IS NOT NULL THEN coalesce('team:'||p.owner_team_id,'personal:'||p.account_id) WHEN t.id IS NOT NULL THEN coalesce('team:'||t.owner_team_id,'personal:'||t.account_id) END) IS NOT NULL
 ON CONFLICT(operation_id) DO NOTHING;
$$;
-- +goose StatementEnd
SELECT paperboat.project_edge_bandwidth(operation_id) FROM paperboat.control_usage_receipts WHERE interval_end>=now()-interval '90 days';
CREATE TABLE paperboat.native_usage_authorities (
 access_session_id text PRIMARY KEY,
 machine_id text NOT NULL,
 installation_generation bigint NOT NULL,
 workspace_id text NOT NULL,
 resource_kind text NOT NULL,
 resource_id text NOT NULL,
 resource_name text NOT NULL,
 resource_route_id text NOT NULL DEFAULT '',
 machine_name text NOT NULL,
 capabilities text[] NOT NULL,
 edge_node_id text NOT NULL DEFAULT '',
 admitted_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL,
 retain_until timestamptz NOT NULL
);
CREATE INDEX native_usage_authorities_retention ON paperboat.native_usage_authorities(retain_until);
-- +goose StatementBegin
CREATE FUNCTION paperboat.snapshot_native_usage_authority() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO paperboat.native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,resource_route_id,machine_name,capabilities,admitted_at,expires_at,retain_until)
 SELECT NEW.id,m.id,m.installation_generation,
 CASE WHEN pub.id IS NOT NULL THEN coalesce('team:'||pub.owner_team_id,'personal:'||pub.account_id) WHEN t.id IS NOT NULL THEN coalesce('team:'||t.owner_team_id,'personal:'||t.owner_account) ELSE coalesce('team:'||m.owner_team_id,'personal:'||m.user_id) END,
 CASE WHEN pub.id IS NOT NULL THEN 'tunnel' WHEN NEW.machine_service_source_kind='lazy_policy' THEN 'lazy_policy' WHEN NEW.machine_service_port IS NOT NULL THEN 'machine_service' WHEN t.id IS NOT NULL THEN 'terminal' ELSE 'machine' END,
 coalesce(pub.id,CASE WHEN NEW.machine_service_source_kind='lazy_policy' THEN NEW.machine_service_source_id END,t.id,m.id),
 coalesce(pub.name,CASE WHEN NEW.machine_service_port IS NOT NULL THEN m.alias||':'||NEW.machine_service_port END,t.title,t.name,m.alias),
 CASE WHEN NEW.machine_service_port IS NOT NULL THEN 'tcp:'||NEW.machine_service_port ELSE '' END,m.alias,
 CASE WHEN t.id IS NOT NULL AND cardinality(NEW.capabilities)=0 THEN ARRAY['terminal'] ELSE NEW.capabilities END,NEW.created_at,least(NEW.expires_at,coalesce(NEW.revoked_at,NEW.expires_at)),NEW.expires_at+interval '7 days'
 FROM paperboat.user_machines m
 LEFT JOIN paperboat.user_machine_terminal_sessions t ON t.id=coalesce(NEW.terminal_session_id,nullif(NEW.operation_id,'')) AND t.user_machine_id=m.id
 LEFT JOIN paperboat.tunnels pub ON NEW.machine_service_source_kind='tunnel' AND pub.id=NEW.machine_service_source_id AND pub.created_by_host_id=m.id
 WHERE m.id=NEW.user_machine_id
 ON CONFLICT(access_session_id) DO UPDATE SET
 expires_at=EXCLUDED.expires_at,retain_until=greatest(native_usage_authorities.retain_until,EXCLUDED.retain_until);
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER native_usage_admission AFTER INSERT OR UPDATE OF expires_at,revoked_at ON paperboat.user_machine_access_sessions FOR EACH ROW EXECUTE FUNCTION paperboat.snapshot_native_usage_authority();
-- Existing sessions are intentionally not reconstructed: their original machine
-- generation is not retained. Only subsequent issued authority is reportable.
CREATE TABLE paperboat.native_usage_reporting_status (
 machine_id text PRIMARY KEY,
 installation_generation bigint NOT NULL,
 workspace_id text NOT NULL,
 last_seen timestamptz NOT NULL,
 unrecorded_bytes bigint NOT NULL CHECK(unrecorded_bytes>=0),
 expired_bytes bigint NOT NULL CHECK(expired_bytes>=0),
 recording_incomplete boolean NOT NULL DEFAULT false,
 rejected_reports bigint NOT NULL DEFAULT 0 CHECK(rejected_reports>=0)
);
-- +goose StatementBegin
CREATE FUNCTION paperboat.snapshot_inspector_usage_authority() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO paperboat.native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,machine_name,capabilities,admitted_at,expires_at,retain_until)
 SELECT NEW.credential_id,m.id,m.installation_generation,
 coalesce('team:'||coalesce(p.owner_team_id,t.owner_team_id),'personal:'||coalesce(p.account_id,t.account_id)),
 NEW.resource_kind,NEW.resource_id,coalesce(p.endpoint,t.name),m.alias,ARRAY['inspector'],NEW.issued_at,NEW.expires_at,NEW.expires_at+interval '7 days'
 FROM paperboat.user_machines m
 LEFT JOIN paperboat.preview_leases p ON NEW.resource_kind='preview' AND p.id=NEW.resource_id AND p.owner_machine_id=m.id
 LEFT JOIN paperboat.tunnels t ON NEW.resource_kind='tunnel' AND t.id=NEW.resource_id AND t.created_by_host_id=m.id
 WHERE p.id IS NOT NULL OR t.id IS NOT NULL ON CONFLICT(access_session_id) DO NOTHING;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER inspector_usage_admission AFTER INSERT ON paperboat.inspector_credentials FOR EACH ROW EXECUTE FUNCTION paperboat.snapshot_inspector_usage_authority();
-- +goose Down
DROP TRIGGER inspector_usage_admission ON paperboat.inspector_credentials;
DROP FUNCTION paperboat.snapshot_inspector_usage_authority();
DROP TABLE paperboat.native_usage_reporting_status;
DROP TRIGGER native_usage_admission ON paperboat.user_machine_access_sessions;
DROP FUNCTION paperboat.snapshot_native_usage_authority();
DROP TABLE paperboat.native_usage_authorities;
DROP FUNCTION paperboat.project_edge_bandwidth(text);
DROP TABLE paperboat.resource_bandwidth_events;
