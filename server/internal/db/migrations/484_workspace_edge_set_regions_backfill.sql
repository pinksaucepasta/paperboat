-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
-- Each stored pair of node ids becomes the pair of their regions, in order (a pair whose two nodes share a region
-- becomes that one region). Batches of 1000 rows walk the table by key, so each batch is short and the walk ends even
-- for rows whose node ids resolve to no region (they keep empty regions and the cursor moves past them). Each batch
-- commits, so row locks are held for one batch only.
-- +goose StatementBegin
DO $$
DECLARE
    cursor_kind text := '';
    cursor_id text := '';
BEGIN
    LOOP
        WITH batch AS (
            SELECT s.scope_kind, s.scope_id FROM paperboat.workspace_edge_sets s
            WHERE (s.scope_kind, s.scope_id) > (cursor_kind, cursor_id) AND cardinality(s.regions) = 0 AND s.node_ids IS NOT NULL
            ORDER BY s.scope_kind, s.scope_id LIMIT 1000),
        done AS (
            UPDATE paperboat.workspace_edge_sets s SET regions = coalesce((
                SELECT (array_agg(first_regions.region ORDER BY first_regions.position))[1:2]
                FROM (SELECT n.region, min(member.position) AS position
                      FROM unnest(s.node_ids) WITH ORDINALITY AS member(id, position)
                      JOIN paperboat.control_tunnel_nodes n ON n.id = member.id AND n.region IS NOT NULL
                      GROUP BY n.region) first_regions), '{}')
            FROM batch b WHERE s.scope_kind = b.scope_kind AND s.scope_id = b.scope_id
            RETURNING s.scope_kind, s.scope_id)
        SELECT d.scope_kind, d.scope_id INTO cursor_kind, cursor_id FROM done d ORDER BY d.scope_kind DESC, d.scope_id DESC LIMIT 1;
        EXIT WHEN NOT FOUND;
        COMMIT;
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
