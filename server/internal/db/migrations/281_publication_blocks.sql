-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Operator takedowns of one hostname (edge abuse plan 3.6.3). While blocked, the server withholds the
-- name's ingress decisions and lists it in the edge snapshot, so the edge answers the fixed takedown page
-- (451 for legal, 403 otherwise) without contacting the origin. The row is kept after an unblock, so the
-- evidence and history stay; version is the operator action revision.
CREATE TABLE paperboat.publication_blocks (
    hostname text PRIMARY KEY CHECK (char_length(hostname) BETWEEN 1 AND 253),
    blocked boolean NOT NULL,
    legal boolean NOT NULL DEFAULT false,
    reason text NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 512),
    account_id text,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX publication_blocks_blocked ON paperboat.publication_blocks(hostname) WHERE blocked;

-- +goose Down
DROP TABLE paperboat.publication_blocks;
