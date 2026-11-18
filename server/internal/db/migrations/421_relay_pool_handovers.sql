-- +goose Up
-- A relay-pool change is make-before-break: the relays an account's pool no longer includes keep serving its
-- carriers until close_at (the account's daemons move to the new relays meanwhile), then close them and refuse the
-- account until its old grants have expired. The relay reads its rows in every observation and the control plane
-- drops each row once that is over. A relay node is never hard-deleted while an account lists it, so the foreign keys
-- only clear the rows of a removed account or node.
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);

CREATE TABLE paperboat.relay_pool_handovers (
    node_id text NOT NULL REFERENCES paperboat.control_tunnel_nodes(id) ON DELETE CASCADE,
    account_id text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
    close_at timestamptz NOT NULL,
    PRIMARY KEY (node_id, account_id)
);

-- +goose Down
DROP TABLE paperboat.relay_pool_handovers;
