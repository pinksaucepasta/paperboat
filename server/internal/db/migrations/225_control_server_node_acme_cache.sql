-- +goose Up
-- Shared, encrypted store for the regional server-node certificate (*.<regional_domain>): the worker leader
-- writes it, every node serves it. Values are encrypted with the shared encryption key.
SET LOCAL lock_timeout = '5s';
CREATE TABLE paperboat.control_server_node_acme_cache (
    key text PRIMARY KEY CHECK (octet_length(key) BETWEEN 1 AND 512),
    sealed bytea NOT NULL CHECK (octet_length(sealed) <= 262144),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE paperboat.control_server_node_acme_cache;
