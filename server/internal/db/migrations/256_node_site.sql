-- +goose Up
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
-- The city or site a node shows next to its region (many nodes share a region), set by an operator. Replaces managed_node_authority.site (255), which no managed node of ours has a row for; that column is dropped one deploy later.
ALTER TABLE paperboat.control_tunnel_nodes ADD COLUMN site text;

-- +goose Down
ALTER TABLE paperboat.control_tunnel_nodes DROP COLUMN site;
