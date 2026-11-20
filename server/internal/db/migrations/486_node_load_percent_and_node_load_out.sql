-- +goose Up
-- One load figure per node, shared by the relay candidate list, the wildcard DNS sync and the admin panel: the worst of
-- the node's sustained CPU and its connections as a share of its own declared limit, from its latest self-report
-- (node_capacity). NULL when the node reported nothing in the last 2 minutes: unknown is never treated as overloaded.
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE FUNCTION paperboat.node_load_percent(wanted_node text) RETURNS real
    LANGUAGE sql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
 SELECT CASE WHEN c.reported_at IS NULL OR c.reported_at <= now() - interval '2 minutes' THEN NULL
        ELSE greatest(c.cpu_sustained_percent,
                      CASE WHEN n.capacity_limit > 0 THEN 100.0 * coalesce(c.connections, n.capacity_used) / n.capacity_limit END)::real END
 FROM control_tunnel_nodes n LEFT JOIN node_capacity c ON c.node_id = n.id
 WHERE n.id = wanted_node;
$$;
-- +goose StatementEnd

-- Which ready managed nodes are held out of rotation because they are overloaded: an edge leaves the wildcard DNS
-- names, a relay leaves the relay candidate list. One row per node the load rule has decided on; the decision is stored
-- so a worker leader change does not reset the hysteresis.
CREATE TABLE paperboat.node_load_out (
    node_id text PRIMARY KEY REFERENCES paperboat.control_tunnel_nodes(id) ON DELETE CASCADE,
    out_for_load boolean NOT NULL,
    changed_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE paperboat.node_load_out;
DROP FUNCTION paperboat.node_load_percent(text);
