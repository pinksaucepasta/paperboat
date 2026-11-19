-- +goose Up
-- small-table: one row per self-hosted node, its latest update request; the next request replaces it once
-- it has finished. An operator presses Update, the node's pbh picks the request up at its next check-in
-- (never an inbound connection), reports each step, and the console/admin show the row.
SET LOCAL lock_timeout = '5s';
CREATE TABLE paperboat.selfhost_update_requests (
    node_id text PRIMARY KEY,
    id text NOT NULL UNIQUE,
    target_version text NOT NULL CHECK (length(target_version) BETWEEN 1 AND 64),
    requested_by text NOT NULL,
    state text NOT NULL CHECK (state IN ('requested', 'downloading', 'verifying', 'swapping', 'checking', 'succeeded', 'rolled_back', 'failed')),
    detail text NOT NULL DEFAULT '' CHECK (octet_length(detail) <= 1024),
    support_reference text NOT NULL DEFAULT '' CHECK (length(support_reference) <= 128),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE paperboat.selfhost_update_requests;
