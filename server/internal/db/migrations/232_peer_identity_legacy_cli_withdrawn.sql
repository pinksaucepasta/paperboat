-- +goose Up
-- A CLI identity from before daemon generations (endpoint_id = the session) has no process left to use it: it is
-- withdrawn, so machines drop that peer.
UPDATE paperboat.peer_network_identities SET revoked_at = now(), revocation_reason = 'withdrawn'
WHERE role = 'cli' AND cli_client_session_id IS NULL AND revoked_at IS NULL;

-- +goose Down
SELECT 1;
