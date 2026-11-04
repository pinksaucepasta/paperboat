-- +goose Up
-- Pending certificate distribution actions and their edge results, shared by every server instance (F21-52):
-- an edge may pull or acknowledge on any instance. Key material is sealed with the certificate master key.
SET LOCAL lock_timeout = '5s';
CREATE TABLE paperboat.tunnel_certificate_distribution_actions (
    action_key text PRIMARY KEY CHECK (action_key ~ '^[0-9a-f]{64}$'),
    edge_node_id text NOT NULL CHECK (octet_length(edge_node_id) BETWEEN 1 AND 256),
    edge_process_epoch text NOT NULL CHECK (octet_length(edge_process_epoch) BETWEEN 1 AND 256),
    message jsonb NOT NULL,
    master_key_reference text NOT NULL DEFAULT '',
    sealed_certificate bytea,
    sealed_private_key bytea,
    status text CHECK (status IN ('ready', 'active', 'retired', 'revoked', 'failed')),
    code text CHECK (code IS NULL OR octet_length(code) BETWEEN 1 AND 128),
    expires_at timestamptz NOT NULL,
    completed_at timestamptz,
    CHECK ((status IS NULL) = (completed_at IS NULL))
);
CREATE INDEX tunnel_certificate_distribution_actions_pending
    ON paperboat.tunnel_certificate_distribution_actions (edge_node_id, edge_process_epoch, action_key)
    WHERE status IS NULL;
CREATE INDEX tunnel_certificate_distribution_actions_retention
    ON paperboat.tunnel_certificate_distribution_actions (coalesce(completed_at, expires_at));

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE paperboat.tunnel_certificate_distribution_actions;
