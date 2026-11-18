-- +goose Up
-- A tunnel's address change is make-before-break: the new name is published to the edges next to the old one
-- ('incoming', until the move times out), and once every live edge serves it the old name is retired and held
-- for a cooldown ('retired', until it may be taken by another resource) so a visitor with the old link cannot
-- land on someone else's tunnel. No foreign key to tunnels: a tunnel is never hard-deleted, the settle worker
-- drops the hold of a deleted one.
SET lock_timeout = '5s';

CREATE TABLE paperboat.tunnel_address_holds (
    hostname text PRIMARY KEY CHECK (hostname = lower(hostname) AND hostname ~ '^[a-z0-9-]+[.][a-z0-9.-]+$'),
    tunnel_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('incoming','retired')),
    until timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One move at a time per tunnel.
CREATE UNIQUE INDEX tunnel_address_holds_incoming ON paperboat.tunnel_address_holds (tunnel_id) WHERE kind = 'incoming';
CREATE INDEX tunnel_address_holds_kind_until ON paperboat.tunnel_address_holds (kind, until);

-- +goose Down
DROP TABLE paperboat.tunnel_address_holds;
