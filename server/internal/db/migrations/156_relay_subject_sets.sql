-- +goose Up
-- Relay observations (PLAN 3b): a relay with thousands of connections sends its full admitted subject list only when
-- the list changed or the server asked for it. Otherwise it sends the list's digest and the server answers from the
-- list stored here for the node's current generation and epoch.
CREATE TABLE paperboat.relay_subject_sets (
    node_id text NOT NULL REFERENCES paperboat.control_tunnel_nodes(id) ON DELETE CASCADE,
    process_epoch text NOT NULL,
    node_generation bigint NOT NULL CHECK (node_generation > 0),
    -- Lowercase hex SHA-256 of the canonical list (relaySubjectsDigest), verified when the list is stored.
    digest text NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    subjects jsonb NOT NULL CHECK (jsonb_typeof(subjects) = 'array'),
    stored_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, process_epoch, node_generation)
);

-- +goose Down
DROP TABLE paperboat.relay_subject_sets;
