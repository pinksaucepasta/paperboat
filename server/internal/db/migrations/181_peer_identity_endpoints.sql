-- +goose Up
-- Direct paths without a relay (PLAN 3b, N5): an engine reports its own candidate endpoints (ip:port, at most 8)
-- with its peer configuration request; the server keeps the normalized set on its identity, written only when
-- it changes, and delivers it on the identity's peer entry (a later step).
ALTER TABLE paperboat.peer_network_identities ADD COLUMN endpoints text[]
    CHECK (endpoints IS NULL OR cardinality(endpoints) <= 8);

-- +goose Down
ALTER TABLE paperboat.peer_network_identities DROP COLUMN endpoints;
