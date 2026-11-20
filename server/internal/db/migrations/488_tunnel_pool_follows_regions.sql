-- +goose NO TRANSACTION
-- +goose Up
-- A self-hosted-only tunnel pool follows its workspace's regions: a sibling installation enrolled into the pair's region
-- before the region backfill (484) was not on the pool's allow-list. Same rule as selfhost.syncTunnelPoolSQL; a
-- workspace without regions keeps its policy. Idempotent: rerunning writes the same lists. Batches of 500 policies
-- walk the table by key and each commits, so row locks are held for one batch only.
SET lock_timeout = '5s';
-- A pool that would become empty keeps its list: self-hosted-only needs a server (SetPool refuses an empty one), and a
-- pair whose regions hold no active server is replaced by the edge-set worker, not by emptying the owner's choice here.
-- +goose StatementBegin
DO $$
DECLARE
    cursor_kind text := '';
    cursor_id text := '';
    prev_kind text;
    prev_id text;
BEGIN
    LOOP
        prev_kind := cursor_kind;
        prev_id := cursor_id;
        SELECT q.scope_kind, q.scope_id INTO cursor_kind, cursor_id FROM (
            SELECT q.scope_kind, q.scope_id FROM paperboat.selfhost_pool_policies q
            WHERE q.capability='tunnel' AND q.mode='self-hosted-only' AND (q.scope_kind, q.scope_id) > (cursor_kind, cursor_id)
            ORDER BY q.scope_kind, q.scope_id LIMIT 500) q
        ORDER BY q.scope_kind DESC, q.scope_id DESC LIMIT 1;
        EXIT WHEN NOT FOUND;
        UPDATE paperboat.selfhost_pool_policies p SET installation_ids=c.ids
        FROM (SELECT q.scope_kind, q.scope_id, ARRAY(
         SELECT i.id FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id
         WHERE i.capability='tunnel' AND i.state='active' AND i.claim_complete AND i.scope_kind<>'global'
          AND ((i.scope_kind=q.scope_kind AND i.scope_id=q.scope_id) OR (q.scope_kind='account' AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,q.scope_id)))
          AND n.region=ANY(s.regions) ORDER BY i.id LIMIT 32) AS ids
         FROM paperboat.selfhost_pool_policies q JOIN paperboat.workspace_edge_sets s ON s.scope_kind=q.scope_kind AND s.scope_id=q.scope_id
         WHERE q.capability='tunnel' AND q.mode='self-hosted-only' AND cardinality(s.regions)>0) c
        WHERE p.capability='tunnel' AND p.scope_kind=c.scope_kind AND p.scope_id=c.scope_id AND cardinality(c.ids)>0
          AND (p.scope_kind, p.scope_id) <= (cursor_kind, cursor_id)
          AND (p.scope_kind, p.scope_id) > (prev_kind, prev_id);
        COMMIT;
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
