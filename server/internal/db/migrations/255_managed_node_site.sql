-- +goose Up
SET lock_timeout = '5s';
-- The city or site a managed node runs in, shown next to its region (many nodes share a region). Self-hosted nodes show their installation's name instead.
ALTER TABLE paperboat.managed_node_authority ADD COLUMN site text;

-- +goose Down
ALTER TABLE paperboat.managed_node_authority DROP COLUMN site;
