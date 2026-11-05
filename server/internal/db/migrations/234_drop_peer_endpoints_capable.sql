-- +goose Up
-- Every engine is given its peers' endpoints from its first configuration (F20-02/04), so nothing records which
-- engines "accept" them any more. Drops what migration 186 added.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.peer_network_identities DROP COLUMN endpoints_capable;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.peer_network_identities ADD COLUMN endpoints_capable boolean NOT NULL DEFAULT false;
