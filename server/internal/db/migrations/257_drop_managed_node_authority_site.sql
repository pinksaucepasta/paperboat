-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Unused since 2026.11.09.22: the site lives on control_tunnel_nodes (256).
ALTER TABLE paperboat.managed_node_authority DROP COLUMN site;

-- +goose Down
ALTER TABLE paperboat.managed_node_authority ADD COLUMN site text;
