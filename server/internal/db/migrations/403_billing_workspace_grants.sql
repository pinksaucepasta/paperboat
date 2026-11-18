-- +goose Up
SET LOCAL lock_timeout = '5s';
-- An operator's entitlement on one workspace ("personal:<account>" or "team:<team>"): extra bandwidth (kind bandwidth,
-- amount in bytes, added to the allowance) or an override of one plan limit (amount is the limit, replacing the plan's
-- value). It runs from creation until ends_at (forever when NULL) or until it is revoked; a row is never deleted, so the
-- audit trail keeps what was granted. At most one running override per workspace and kind is kept by the operator action;
-- bandwidth grants add up.
CREATE TABLE paperboat.billing_workspace_grants (
    id text PRIMARY KEY,
    workspace_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('bandwidth','machines','publications','browser_terminals','public_tcp','seats','viewers_per_terminal')),
    amount bigint NOT NULL CHECK (amount >= 0),
    ends_at timestamptz,
    reason text NOT NULL,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    revoked_by text,
    CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);
CREATE INDEX billing_workspace_grants_workspace ON paperboat.billing_workspace_grants (workspace_id, kind, created_at DESC);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE paperboat.billing_workspace_grants;
