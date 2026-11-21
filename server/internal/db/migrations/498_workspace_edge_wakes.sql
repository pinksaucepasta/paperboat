-- +goose Up
-- Outbox of workspaces whose machines must look at their edges again. The transaction that changes a workspace's
-- edge pair inserts a row (no read of its machines, so no predicate locks on them); a drainer wakes the machines and
-- deletes the row in one read-committed transaction, so a crash between the pair's commit and the wake loses nothing.
SET LOCAL lock_timeout = '5s';
CREATE TABLE paperboat.workspace_edge_wakes (
    scope_kind text NOT NULL CHECK (scope_kind IN ('account', 'team')),
    scope_id text NOT NULL,
    queued_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope_kind, scope_id)
);

-- +goose Down
DROP TABLE paperboat.workspace_edge_wakes;
