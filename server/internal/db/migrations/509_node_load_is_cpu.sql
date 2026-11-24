-- +goose Up
-- A node's load is its sustained CPU from its latest self-report. A relay or edge has no connection limit, so
-- control_tunnel_nodes.capacity_limit (a declared size nothing enforced) no longer feeds the load (487 used the worse of the two).
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.node_load_percent(wanted_node text) RETURNS real
    LANGUAGE sql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
 SELECT CASE WHEN c.reported_at IS NULL OR c.reported_at <= now() - interval '2 minutes' THEN NULL
       ELSE c.cpu_sustained_percent::real END
 FROM control_tunnel_nodes n LEFT JOIN node_capacity c ON c.node_id = n.id
 WHERE n.id = wanted_node;
$$;
-- +goose StatementEnd

-- +goose Down
-- Forward only (zero users).
SELECT 1;
