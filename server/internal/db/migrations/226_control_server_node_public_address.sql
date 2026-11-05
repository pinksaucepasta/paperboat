-- +goose Up
-- The public address DNS publishes for a node, declared by the node and approved by the enrolling operator; it
-- may differ from the address the operator dialed (a node reached over a private network).
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.control_server_nodes ADD COLUMN public_address text CHECK (octet_length(public_address) BETWEEN 1 AND 64);

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.control_server_nodes DROP COLUMN public_address;
