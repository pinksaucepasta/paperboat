-- +goose Up
-- The edges serving a tunnel route, read by the certificate worker to place and confirm a durable
-- domain's certificate. Connector-path routes come from tunnel_edge_route_assignments; a tunnel moved
-- onto the endpoint engine (PLAN 3a step 4.3) has no connector session and is served by its
-- endpoint_edge_assignments. Both rows carry the assignment generation the edge acknowledges, so
-- the certificate ledger fences distribution the same way for either path. One owner, read by
-- every certificate query on a durable route.
-- +goose StatementBegin
CREATE VIEW paperboat.tunnel_route_ready_edges_v1 AS
SELECT a.account_id, a.tunnel_id, a.route_id, a.edge_node_id, a.edge_process_epoch, a.assignment_generation
FROM paperboat.tunnel_edge_route_assignments AS a
WHERE a.state = 'active' AND a.observed_state = 'ready'
UNION
SELECT e.account_id, e.tunnel_id, er.tunnel_route_id, ea.edge_node_id, ea.edge_process_epoch, ea.generation
FROM paperboat.endpoint_edge_assignments AS ea
JOIN paperboat.endpoint_routes AS er ON er.id = ea.route_id
JOIN paperboat.endpoints AS e ON e.id = er.endpoint_id
WHERE e.kind = 'tunnel' AND e.desired_state = 'active' AND ea.desired = 'active' AND ea.observed_ready;
-- +goose StatementEnd

-- +goose Down
DROP VIEW paperboat.tunnel_route_ready_edges_v1;
