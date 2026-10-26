-- +goose Up
-- When an edge process last started fetching its complete tunnel route snapshot. A tunnel pause or
-- deletion is complete only once every live edge fetched a snapshot that began after the change, so
-- `pb tunnel pause --wait` returns when the edges have dropped the route, not before.
ALTER TABLE paperboat.control_tunnel_nodes ADD COLUMN tunnel_snapshot_at timestamptz;

-- +goose Down
ALTER TABLE paperboat.control_tunnel_nodes DROP COLUMN tunnel_snapshot_at;
