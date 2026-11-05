-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.peer_network_identities VALIDATE CONSTRAINT peer_network_identities_owner_check;
ALTER TABLE paperboat.peer_network_identities VALIDATE CONSTRAINT peer_network_identities_revocation_reason_check;

-- +goose Down
SELECT 1;
