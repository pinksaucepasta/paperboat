-- +goose Up
-- Peer network deltas (PLAN 3b): the personal mesh of an account (every endpoint of it is a peer of every other)
-- was rebuilt by every endpoint on every change, N x N per join. It is now built once per change into a shared,
-- ordered per-account log; an endpoint asks for the entries after the mesh_seq it holds.
CREATE TABLE paperboat.peer_mesh_accounts (
    account_id text PRIMARY KEY REFERENCES paperboat.users(id) ON DELETE CASCADE,
    mesh_seq bigint NOT NULL DEFAULT 0 CHECK (mesh_seq >= 0),
    -- Entries at or below pruned_seq may be gone: a base older than it gets a snapshot.
    pruned_seq bigint NOT NULL DEFAULT 0 CHECK (pruned_seq >= 0),
    -- The account's peer_network + inventory topic generations when the mesh was built (the change signal).
    built_signal bigint NOT NULL DEFAULT 0,
    built_at timestamptz,
    -- The earliest certificate expiry in the mesh: the mesh is rebuilt when it passes.
    valid_until timestamptz
);

CREATE TABLE paperboat.peer_mesh_log (
    account_id text NOT NULL REFERENCES paperboat.peer_mesh_accounts(account_id) ON DELETE CASCADE,
    mesh_seq bigint NOT NULL CHECK (mesh_seq > 0),
    endpoint_id text NOT NULL,
    op text NOT NULL CHECK (op IN ('add', 'update', 'remove')),
    entry jsonb CHECK ((op = 'remove') = (entry IS NULL)),
    recorded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, mesh_seq)
);
-- The newest entry of each endpoint at or before a seq (a base's view) and the entries after a seq.
CREATE INDEX peer_mesh_log_endpoint ON paperboat.peer_mesh_log (account_id, endpoint_id, mesh_seq);

-- +goose Down
DROP TABLE paperboat.peer_mesh_log;
DROP TABLE paperboat.peer_mesh_accounts;
