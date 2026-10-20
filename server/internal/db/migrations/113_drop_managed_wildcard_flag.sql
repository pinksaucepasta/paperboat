-- +goose Up
-- The flag decided which edges the managed wildcard reaches while edges were not interchangeable.
-- Placement now follows each workspace's edge set (103) and no code reads the column.
ALTER TABLE paperboat.control_tunnel_nodes DROP COLUMN serves_managed_wildcard;

-- +goose Down
ALTER TABLE paperboat.control_tunnel_nodes ADD COLUMN serves_managed_wildcard boolean NOT NULL DEFAULT true;
