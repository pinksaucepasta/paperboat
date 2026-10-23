-- +goose Up
-- Edge bandwidth of a preview served as an endpoint route (PLAN 3a) was not attributed to the preview:
-- the projection found previews only through the legacy preview_lease_carrier_attachments. An
-- endpoint route's id is its preview's id, and endpoint_routes outlives the edge observation, so the
-- projection also resolves it there.
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
 LEFT JOIN LATERAL (SELECT preview_id FROM (
  (SELECT preview_id FROM paperboat.preview_lease_carrier_attachments WHERE route_id=r.route_id AND edge_node_id=r.edge_node_id AND route_generation=r.route_revision ORDER BY created_at DESC LIMIT 1)
  UNION ALL
  (SELECT e.preview_lease_id FROM paperboat.endpoint_routes er JOIN paperboat.endpoints e ON e.id=er.endpoint_id AND e.kind='preview' WHERE er.id=r.route_id AND er.revision=r.route_revision LIMIT 1)
 ) found LIMIT 1) pa ON true
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

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'edge bandwidth projection migration is forward-only'; END $$;
-- +goose StatementEnd
