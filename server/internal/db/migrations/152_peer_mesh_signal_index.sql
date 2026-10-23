-- +goose Up
-- The shared peer mesh's change signal (peersessions/mesh.go meshSignal) sums the account's identities' relay
-- revocation generations on every refresh: an index-only scan over the account instead of its heap rows.
CREATE INDEX peer_network_identities_user_relay_generation ON paperboat.peer_network_identities (user_id) INCLUDE (relay_revocation_generation);

-- +goose Down
DROP INDEX paperboat.peer_network_identities_user_relay_generation;
