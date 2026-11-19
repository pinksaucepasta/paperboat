-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.control_tunnel_nodes VALIDATE CONSTRAINT control_tunnel_nodes_public_https_port_check;

-- +goose Down
SELECT 1;
