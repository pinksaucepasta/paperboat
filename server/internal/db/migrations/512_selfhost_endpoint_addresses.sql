-- +goose Up
-- The addresses the server publishes for the name it gave this installation's endpoint (selfhost.NodeNames), sorted;
-- NULL when the endpoint host is the operator's own name or an IP.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.selfhost_installations ADD COLUMN endpoint_addresses text[];

-- +goose Down
ALTER TABLE paperboat.selfhost_installations DROP COLUMN endpoint_addresses;
