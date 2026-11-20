-- +goose NO TRANSACTION
-- +goose Up
-- A self-hosted-only tunnel pool follows its workspace's regions, including sibling installations
-- in those regions. Keep the existing policy if there are no regions or no eligible installations.
-- +goose StatementBegin
DO $$
DECLARE
    cursor_kind text := '';
    cursor_id text := '';
BEGIN
    LOOP
        SET LOCAL lock_timeout = '5s';
        WITH batch AS (
            SELECT q.scope_kind, q.scope_id, s.regions
            FROM paperboat.selfhost_pool_policies q
            JOIN paperboat.workspace_edge_sets s ON s.scope_kind = q.scope_kind AND s.scope_id = q.scope_id
            WHERE (q.scope_kind, q.scope_id) > (cursor_kind, cursor_id)
              AND q.capability = 'tunnel' AND q.mode = 'self-hosted-only' AND cardinality(s.regions) > 0
            ORDER BY q.scope_kind, q.scope_id LIMIT 500),
        candidates AS (
            SELECT b.scope_kind, b.scope_id, ARRAY(
                SELECT i.id FROM paperboat.selfhost_installations i
                JOIN paperboat.control_tunnel_nodes n ON n.id = i.node_id
                WHERE i.capability = 'tunnel' AND i.state = 'active' AND i.claim_complete AND i.scope_kind <> 'global'
                  AND ((i.scope_kind = b.scope_kind AND i.scope_id = b.scope_id)
                    OR (b.scope_kind = 'account' AND paperboat.selfhost_scope_visible(i.scope_kind, i.scope_id, b.scope_id)))
                  AND n.region = ANY(b.regions)
                ORDER BY i.id LIMIT 32) AS ids
            FROM batch b),
        updated AS (
            UPDATE paperboat.selfhost_pool_policies p SET installation_ids = c.ids
            FROM candidates c
            WHERE p.scope_kind = c.scope_kind AND p.scope_id = c.scope_id
              AND p.capability = 'tunnel' AND p.mode = 'self-hosted-only'
              AND cardinality(c.ids) > 0 AND p.installation_ids IS DISTINCT FROM c.ids)
        SELECT b.scope_kind, b.scope_id INTO cursor_kind, cursor_id
        FROM batch b ORDER BY b.scope_kind DESC, b.scope_id DESC LIMIT 1;
        EXIT WHEN NOT FOUND;
        COMMIT;
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
