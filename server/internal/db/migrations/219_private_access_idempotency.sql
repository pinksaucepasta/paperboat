-- +goose Up
-- One body per private-access idempotency key across every server instance (F21-54): the same key with
-- a different request is refused wherever it arrives. Rows live as long as the decision (minutes).
SET LOCAL lock_timeout = '5s';
CREATE TABLE paperboat.private_access_idempotency (
    account_id text NOT NULL CHECK (octet_length(account_id) BETWEEN 1 AND 256),
    session_id text NOT NULL CHECK (octet_length(session_id) BETWEEN 1 AND 256),
    idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 256),
    fingerprint text NOT NULL CHECK (octet_length(fingerprint) BETWEEN 1 AND 128),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, session_id, idempotency_key)
);
CREATE INDEX private_access_idempotency_expires ON paperboat.private_access_idempotency (expires_at);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE paperboat.private_access_idempotency;
