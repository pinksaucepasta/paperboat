-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Operator deny list of client networks: sign-up, sign-in and CLI login from an address inside a running block are refused.
-- A block runs until expires_at (forever when NULL) or until it is revoked; a row is never deleted, so the audit trail keeps
-- what was blocked and why. One running block per exact range.
CREATE TABLE paperboat.ip_blocks (
    id text PRIMARY KEY,
    seq bigint GENERATED ALWAYS AS IDENTITY,
    cidr cidr NOT NULL,
    reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 512),
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    revoked_at timestamptz,
    revoked_by text,
    CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);
CREATE UNIQUE INDEX ip_blocks_cidr_running ON paperboat.ip_blocks (cidr) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX ip_blocks_seq ON paperboat.ip_blocks (seq DESC);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE paperboat.ip_blocks;
