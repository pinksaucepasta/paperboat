-- +goose Up
-- A removed route kept its name forever, so adding the same hostname and path again failed with
-- "the route conflicts with an existing route" (console names routes from their path). Names only
-- have to be unique among routes that still exist.
ALTER TABLE paperboat.tunnel_routes DROP CONSTRAINT tunnel_routes_tunnel_id_name_key;
CREATE UNIQUE INDEX tunnel_routes_tunnel_id_name_key ON paperboat.tunnel_routes (tunnel_id, name) WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX paperboat.tunnel_routes_tunnel_id_name_key;
ALTER TABLE paperboat.tunnel_routes ADD CONSTRAINT tunnel_routes_tunnel_id_name_key UNIQUE (tunnel_id, name);
