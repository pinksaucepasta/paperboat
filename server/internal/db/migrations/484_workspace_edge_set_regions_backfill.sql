-- +goose NO TRANSACTION
-- +goose Up
-- Each stored pair of node ids becomes the pair of their regions, in order (a pair whose two nodes share a region
-- becomes that one region). Commit each batch of up to 1000 rows to release its locks. The cursor advances even
-- for rows whose node ids resolve to no region (they keep empty regions and the cursor moves past them).
-- +goose StatementBegin
DO $$
DECLARE
    cursor_kind text := '';
    cursor_id text := '';
BEGIN
    LOOP
        SET LOCAL lock_timeout = '5s';
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
              AND cardinality(s.regions) = 0 AND s.node_ids IS NOT NULL)
        SELECT b.scope_kind, b.scope_id INTO cursor_kind, cursor_id
        FROM batch b ORDER BY b.scope_kind DESC, b.scope_id DESC LIMIT 1;
        EXIT WHEN NOT FOUND;
        COMMIT;
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
