-- +goose Up
SET LOCAL lock_timeout = '5s';
-- One break-glass run: the operator action that rotates every key machines trust, re-seals stored secrets and signs
-- everyone out after a server breach (internal/breakglass). The run row is the whole state: a worker advances the
-- current stage one bounded step at a time, so a restarted server resumes where the crashed one stopped. Only one
-- run is active at a time. Runs are started by an operator, rarely, and are kept as the record of what was done.
CREATE TABLE paperboat.breakglass_runs (
    id text PRIMARY KEY,
    state text NOT NULL CHECK (state IN ('running', 'done', 'cancelled')),
    operator_id text NOT NULL REFERENCES paperboat.operator_accounts(id),
    reason text NOT NULL CHECK (length(reason) BETWEEN 8 AND 500),
    -- The acting operator's session, kept signed in while every other operator session is revoked.
    keep_session_hash bytea CHECK (octet_length(keep_session_hash) = 32),
    floor_version text NOT NULL DEFAULT '',
    -- What the operator saw before confirming: counts and the key checks.
    preview jsonb NOT NULL,
    -- Per stage: state, times and counts (stage name -> object), in the order they run.
    stages jsonb NOT NULL,
    last_error text NOT NULL DEFAULT '',
    started_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE UNIQUE INDEX breakglass_runs_one_running ON paperboat.breakglass_runs ((true)) WHERE state = 'running';

-- +goose Down
DROP TABLE paperboat.breakglass_runs;
