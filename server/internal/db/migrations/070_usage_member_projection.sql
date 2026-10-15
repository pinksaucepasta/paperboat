-- +goose Up
ALTER TABLE paperboat.native_usage_authorities ADD COLUMN actor_account_id text
 CHECK(actor_account_id IS NULL OR length(actor_account_id) BETWEEN 1 AND 256);
ALTER TABLE paperboat.control_usage_receipts ADD COLUMN actor_account_id text
 CHECK(actor_account_id IS NULL OR length(actor_account_id) BETWEEN 1 AND 256);
ALTER TABLE paperboat.resource_bandwidth_events ADD COLUMN actor_account_id text
 CHECK(actor_account_id IS NULL OR length(actor_account_id) BETWEEN 1 AND 256);
CREATE INDEX resource_bandwidth_member_time
 ON paperboat.resource_bandwidth_events(workspace_id,actor_account_id,interval_end);

-- Exact retained admission rows identify an actor; a team owner is never a proxy.
UPDATE paperboat.native_usage_authorities a SET actor_account_id=s.user_id
 FROM paperboat.user_machine_access_sessions s WHERE s.id=a.access_session_id
 AND s.user_machine_id=a.machine_id;
UPDATE paperboat.native_usage_authorities a SET actor_account_id=s.account_id
 FROM paperboat.inspector_credentials s WHERE s.credential_id=a.access_session_id;
UPDATE paperboat.native_usage_authorities a SET actor_account_id=s.account_id
 FROM paperboat.config_compare_admissions s WHERE s.id=a.access_session_id
 AND s.native_cli_session_id IS NOT NULL;
UPDATE paperboat.control_usage_receipts r SET actor_account_id=a.actor_account_id
 FROM paperboat.usage_accounting_authorities a
 WHERE a.authority_id=r.authority_id AND a.edge_node_id=r.edge_node_id
 AND a.environment_id=r.environment_id AND a.route_id=r.route_id
 AND a.route_revision=r.route_revision;
UPDATE paperboat.resource_bandwidth_events e SET actor_account_id=r.actor_account_id
 FROM paperboat.control_usage_receipts r WHERE e.operation_id='edge:'||r.operation_id;
-- Personal ownership is unambiguous even after the individual authority is pruned.
UPDATE paperboat.resource_bandwidth_events e SET actor_account_id=w.user_id
 FROM paperboat.billing_workspaces w WHERE w.id=e.workspace_id
 AND w.user_id IS NOT NULL AND e.actor_account_id IS NULL;
UPDATE paperboat.resource_bandwidth_events e SET actor_account_id=u.id
 FROM paperboat.users u WHERE e.workspace_id='personal:'||u.id
 AND e.actor_account_id IS NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.project_edge_bandwidth(receipt_operation text) RETURNS void LANGUAGE sql AS $$
 DELETE FROM paperboat.resource_bandwidth_events WHERE operation_id IN (SELECT operation_id FROM paperboat.resource_bandwidth_events WHERE interval_end<now()-interval '90 days' LIMIT 500);
 INSERT INTO paperboat.resource_bandwidth_events(operation_id,workspace_id,resource_kind,resource_id,resource_name,machine_id,machine_name,mode,hosting,node_id,node_name,upload_bytes,download_bytes,interval_start,interval_end,actor_account_id)
 SELECT 'edge:'||r.operation_id,
 coalesce(r.workspace_id,a.workspace_id,CASE WHEN p.id IS NOT NULL THEN coalesce('team:'||p.owner_team_id,'personal:'||p.account_id) WHEN t.id IS NOT NULL THEN coalesce('team:'||t.owner_team_id,'personal:'||t.account_id) END),
 CASE WHEN p.id IS NOT NULL THEN 'preview' WHEN t.id IS NOT NULL THEN 'tunnel' ELSE 'route' END,
 coalesce(p.id,t.id,r.route_id),coalesce(p.endpoint,t.name,r.route_id),coalesce(m.id,''),coalesce(m.alias,''),
 'edge',CASE WHEN i.scope_kind IN ('account','team') THEN 'self_hosted' WHEN n.id IS NOT NULL THEN 'managed' ELSE 'unknown' END,
 r.edge_node_id,coalesce(i.name,r.edge_node_id),CASE WHEN r.direction='ingress' THEN r.delta_bytes ELSE 0 END,CASE WHEN r.direction='egress' THEN r.delta_bytes ELSE 0 END,r.interval_start,r.interval_end,coalesce(r.actor_account_id,a.actor_account_id)
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.snapshot_native_usage_authority() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO paperboat.native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,resource_route_id,machine_name,capabilities,admitted_at,expires_at,retain_until,actor_account_id)
 SELECT NEW.id,m.id,m.installation_generation,
 CASE WHEN pub.id IS NOT NULL THEN coalesce('team:'||pub.owner_team_id,'personal:'||pub.account_id) WHEN t.id IS NOT NULL THEN coalesce('team:'||t.owner_team_id,'personal:'||t.owner_account) ELSE coalesce('team:'||m.owner_team_id,'personal:'||m.user_id) END,
 CASE WHEN pub.id IS NOT NULL THEN 'tunnel' WHEN NEW.machine_service_source_kind='lazy_policy' THEN 'lazy_policy' WHEN NEW.machine_service_port IS NOT NULL THEN 'machine_service' WHEN t.id IS NOT NULL THEN 'terminal' ELSE 'machine' END,
 coalesce(pub.id,CASE WHEN NEW.machine_service_source_kind='lazy_policy' THEN NEW.machine_service_source_id END,t.id,m.id),
 coalesce(pub.name,CASE WHEN NEW.machine_service_port IS NOT NULL THEN m.alias||':'||NEW.machine_service_port END,t.title,t.name,m.alias),
 CASE WHEN NEW.machine_service_port IS NOT NULL THEN 'tcp:'||NEW.machine_service_port ELSE '' END,m.alias,
 CASE WHEN t.id IS NOT NULL AND cardinality(NEW.capabilities)=0 THEN ARRAY['terminal'] ELSE NEW.capabilities END,NEW.created_at,least(NEW.expires_at,coalesce(NEW.revoked_at,NEW.expires_at)),NEW.expires_at+interval '7 days',NEW.user_id
 FROM paperboat.user_machines m
 LEFT JOIN paperboat.user_machine_terminal_sessions t ON t.id=coalesce(NEW.terminal_session_id,nullif(NEW.operation_id,'')) AND t.user_machine_id=m.id
 LEFT JOIN paperboat.tunnels pub ON NEW.machine_service_source_kind='tunnel' AND pub.id=NEW.machine_service_source_id AND pub.created_by_host_id=m.id
 WHERE m.id=NEW.user_machine_id
 ON CONFLICT(access_session_id) DO UPDATE SET
 actor_account_id=coalesce(native_usage_authorities.actor_account_id,EXCLUDED.actor_account_id),
 expires_at=EXCLUDED.expires_at,retain_until=greatest(native_usage_authorities.retain_until,EXCLUDED.retain_until);
 RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.snapshot_inspector_usage_authority() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO paperboat.native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,machine_name,capabilities,admitted_at,expires_at,retain_until,actor_account_id)
 SELECT NEW.credential_id,m.id,m.installation_generation,
 coalesce('team:'||coalesce(p.owner_team_id,t.owner_team_id),'personal:'||coalesce(p.account_id,t.account_id)),
 NEW.resource_kind,NEW.resource_id,coalesce(p.endpoint,t.name),m.alias,ARRAY['inspector'],NEW.issued_at,NEW.expires_at,NEW.expires_at+interval '7 days',NEW.account_id
 FROM paperboat.user_machines m
 LEFT JOIN paperboat.preview_leases p ON NEW.resource_kind='preview' AND p.id=NEW.resource_id AND p.owner_machine_id=m.id
 LEFT JOIN paperboat.tunnels t ON NEW.resource_kind='tunnel' AND t.id=NEW.resource_id AND t.created_by_host_id=m.id
 WHERE p.id IS NOT NULL OR t.id IS NOT NULL ON CONFLICT(access_session_id) DO NOTHING;
 RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.snapshot_config_compare_native_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.native_cli_session_id IS NOT NULL THEN
 INSERT INTO paperboat.native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,machine_name,capabilities,admitted_at,expires_at,retain_until,actor_account_id)
 SELECT NEW.id,m.id,m.installation_generation,coalesce('team:'||a.adopted_team_id,'personal:'||NEW.account_id),'config',a.id,coalesce(r.display_name,'Configuration')||' config',m.alias,ARRAY['config_compare'],now(),NEW.expires_at,NEW.expires_at+interval '7 days',NEW.account_id
 FROM paperboat.control_config_assignments a JOIN paperboat.user_machines m ON m.id=a.machine_id LEFT JOIN paperboat.control_config_repositories r ON r.id=a.repository_id
 WHERE a.id=NEW.assignment_id AND a.version=NEW.assignment_version AND m.id=NEW.machine_id AND m.user_id=NEW.account_id AND m.installation_generation=NEW.installation_generation
 ON CONFLICT(access_session_id) DO NOTHING;
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- Retained usage identities must not be discarded.
