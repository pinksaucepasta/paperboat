-- +goose Up
-- Each stored pair of node ids becomes the pair of their regions, in order (a pair whose two nodes share a region
-- becomes that one region). One row per workspace, one statement.
UPDATE paperboat.workspace_edge_sets s SET regions = coalesce((
    SELECT (array_agg(first_regions.region ORDER BY first_regions.position))[1:2]
    FROM (SELECT n.region, min(member.position) AS position
          FROM unnest(s.node_ids) WITH ORDINALITY AS member(id, position)
          JOIN paperboat.control_tunnel_nodes n ON n.id = member.id AND n.region IS NOT NULL
          GROUP BY n.region) first_regions), '{}')
WHERE cardinality(s.regions) = 0 AND s.node_ids IS NOT NULL;

-- +goose Down
SELECT 1;
