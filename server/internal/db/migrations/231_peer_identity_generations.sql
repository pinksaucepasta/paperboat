-- +goose Up
-- A user daemon generation (one per installed pb binary) has its own network identity: endpoint_id is
-- `<cli session>_<generation>` (shared/peernetwork.DaemonEndpointID), with its own WireGuard key and address, so an
-- upgraded daemon serves new connects while the old one drains what it holds (make-before-break, like the carrier
-- generations). cli_client_session_id names the session of a daemon generation; owner_endpoint_id is what any
-- identity acts for: that session (whose grants and certificate every generation shares) or the identity itself (a
-- machine). drain_until bounds a draining generation (runtimeattachment.DrainLimit); revocation_reason tells a
-- refused generation why.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.peer_network_identities
    ADD COLUMN cli_client_session_id text,
    ADD COLUMN owner_endpoint_id text GENERATED ALWAYS AS (coalesce(cli_client_session_id, endpoint_id)) STORED,
    ADD COLUMN drain_until timestamptz,
    ADD COLUMN revocation_reason text;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.peer_network_identities
    DROP COLUMN revocation_reason,
    DROP COLUMN drain_until,
    DROP COLUMN owner_endpoint_id,
    DROP COLUMN cli_client_session_id;
