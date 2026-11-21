-- +goose Up
-- A managed node is a pbh installation claimed with global scope, in the operator region named by its install; there
-- is no second enrolment path (the operator-minted credential table only staging ever used). The view keeps the
-- columns every reader (serving set, wildcard DNS, eligibility functions, drain, ingress verification) already uses.
SET lock_timeout = '5s';
DROP TABLE paperboat.managed_node_authority;
CREATE VIEW paperboat.managed_node_authority AS
SELECT i.node_id, n.region AS region_id, i.runtime_hash, i.runtime_ciphertext, '{}'::jsonb AS config,
       n.node_generation::bigint AS revision,
       CASE WHEN n.state='retired' THEN n.updated_at END AS retired_at, i.created_at, n.updated_at
FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id
WHERE i.scope_kind='global' AND i.state='active' AND i.claim_complete AND n.region IS NOT NULL;

-- +goose Down
SET lock_timeout = '5s';
DROP VIEW paperboat.managed_node_authority;
CREATE TABLE paperboat.managed_node_authority (
    node_id text PRIMARY KEY REFERENCES paperboat.control_tunnel_nodes(id) ON DELETE CASCADE,
    region_id text NOT NULL REFERENCES paperboat.operator_regions(id),
    runtime_hash bytea NOT NULL UNIQUE CHECK (octet_length(runtime_hash) = 32),
    runtime_ciphertext bytea NOT NULL,
    config jsonb NOT NULL,
    revision bigint DEFAULT 1 NOT NULL CHECK (revision > 0),
    retired_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
