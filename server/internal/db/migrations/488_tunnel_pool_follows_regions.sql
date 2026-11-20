-- +goose Up
-- A self-hosted-only tunnel pool follows its workspace's regions: a sibling installation enrolled into the pair's region
-- before the region backfill (484) was not on the pool's allow-list. Same rule as selfhost.syncTunnelPoolSQL; a
-- workspace without regions keeps its policy. Idempotent: rerunning writes the same lists.
SET LOCAL lock_timeout = '5s';
UPDATE paperboat.selfhost_pool_policies p SET installation_ids=ARRAY(
 SELECT i.id FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id
 WHERE i.capability='tunnel' AND i.state='active' AND i.claim_complete AND i.scope_kind<>'global'
  AND ((i.scope_kind=p.scope_kind AND i.scope_id=p.scope_id) OR (p.scope_kind='account' AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,p.scope_id)))
  AND n.region=ANY(s.regions) ORDER BY i.id LIMIT 32)
FROM paperboat.workspace_edge_sets s
WHERE p.capability='tunnel' AND p.mode='self-hosted-only' AND s.scope_kind=p.scope_kind AND s.scope_id=p.scope_id AND cardinality(s.regions)>0;

-- +goose Down
SELECT 1;
