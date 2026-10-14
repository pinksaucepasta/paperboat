-- +goose Up
ALTER TABLE paperboat.tunnel_edge_route_assignments DROP CONSTRAINT tunnel_edge_route_assignments_observed_state_check;
ALTER TABLE paperboat.tunnel_edge_route_assignments ADD CONSTRAINT tunnel_edge_route_assignments_observed_state_check CHECK (observed_state IN ('pending','prepared','ready','degraded','draining','detached','failed'));

-- +goose Down
UPDATE paperboat.tunnel_edge_route_assignments SET observed_state='pending' WHERE observed_state='prepared';
ALTER TABLE paperboat.tunnel_edge_route_assignments DROP CONSTRAINT tunnel_edge_route_assignments_observed_state_check;
ALTER TABLE paperboat.tunnel_edge_route_assignments ADD CONSTRAINT tunnel_edge_route_assignments_observed_state_check CHECK (observed_state IN ('pending','ready','degraded','draining','detached','failed'));
