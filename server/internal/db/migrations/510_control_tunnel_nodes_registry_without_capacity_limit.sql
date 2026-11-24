-- +goose Up
-- The registry check no longer asks for a declared size (capacity_limit > 0, capacity_used <= capacity_limit), so rows
-- can be written without naming the column. The column stays (NOT NULL DEFAULT 0, which the previous server still reads and
-- writes) and is dropped one deploy after no server names it. The new check is looser than the old one; 511 validates it.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.control_tunnel_nodes
    DROP CONSTRAINT control_tunnel_nodes_regional_registry_check,
    ADD CONSTRAINT control_tunnel_nodes_regional_registry_check CHECK ((((region IS NULL) AND (failure_domain IS NULL) AND (cardinality(roles) = 0) AND (cardinality(transports) = 0) AND (registry_expires_at IS NULL)) OR (((length(region) >= 1) AND (length(region) <= 63)) AND ((length(failure_domain) >= 1) AND (length(failure_domain) <= 128)) AND ((cardinality(roles) >= 1) AND (cardinality(roles) <= 2)) AND (roles <@ ARRAY['edge'::text, 'relay'::text, 'peer_relay'::text]) AND ((cardinality(transports) >= 1) AND (cardinality(transports) <= 4)) AND (transports <@ ARRAY['http3'::text, 'http2'::text, 'derp_quic'::text, 'derp_wss'::text, 'peer_relay_udp'::text]) AND (capacity_observed_at IS NOT NULL) AND (registry_expires_at IS NOT NULL)))) NOT VALID;

-- +goose Down
-- Forward only (zero users).
SELECT 1;
