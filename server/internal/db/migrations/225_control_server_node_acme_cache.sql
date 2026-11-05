-- +goose Up
-- Shared ACME cache for regional server-node names: every node of a region reads the same certificate and
-- answers HTTP-01 challenges from the same tokens. Values are encrypted with the shared encryption key.
SET LOCAL lock_timeout = '5s';
CREATE TABLE paperboat.control_server_node_acme_cache (
    key text PRIMARY KEY CHECK (octet_length(key) BETWEEN 1 AND 512),
    sealed bytea NOT NULL CHECK (octet_length(sealed) <= 262144),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE paperboat.control_server_node_acme_cache;
