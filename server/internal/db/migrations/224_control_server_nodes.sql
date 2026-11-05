-- +goose Up
-- Server nodes are equal peers (PLAN 3b "Regional control plane"): every node heartbeats its own row here,
-- the shared database is the one record of which nodes exist, and any node's panel reads the same rows.
SET LOCAL lock_timeout = '5s';
CREATE TABLE paperboat.control_server_nodes (
    id text PRIMARY KEY CHECK (octet_length(id) BETWEEN 1 AND 128),
    region text NOT NULL CHECK (region ~ '^[a-z][a-z0-9-]{0,23}$'),
    -- registered: enrolled by an operator, not yet serving; a node that holds database credentials itself is active.
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('registered', 'active', 'draining', 'removed')),
    endpoint_host text CHECK (octet_length(endpoint_host) BETWEEN 1 AND 253),
    tls_spki_sha256 bytea CHECK (octet_length(tls_spki_sha256) = 32),
    enrolled_by text,
    version text NOT NULL CHECK (octet_length(version) BETWEEN 1 AND 64),
    commit text NOT NULL CHECK (octet_length(commit) BETWEEN 1 AND 64),
    started_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX control_server_nodes_region ON paperboat.control_server_nodes (region, last_seen_at);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE paperboat.control_server_nodes;
