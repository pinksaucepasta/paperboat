-- +goose Up
-- PLAN 3a step 4: tunnels on the endpoint engine. A tunnel whose agent and every edge the agent holds
-- carriers to advertise 'tunnel_endpoints' is served as an endpoint: its routes ride on the agent's
-- runtime carriers instead of the tunnel's own connector. The tunnel, its routes, domains and
-- config stay owned by tunnels/tunnel_routes; the endpoint rows only anchor edge assignments.

ALTER TABLE paperboat.runtime_carrier_attachments
    DROP CONSTRAINT runtime_carrier_attachments_agent_capabilities_check,
    ADD CONSTRAINT runtime_carrier_attachments_agent_capabilities_check
    CHECK (agent_capabilities <@ ARRAY['endpoints', 'tunnel_endpoints']::text[]);

-- The edge process epoch that last advertised 'tunnel_endpoints' (like endpoints_epoch).
ALTER TABLE paperboat.control_tunnel_nodes ADD COLUMN tunnel_endpoints_epoch text;

ALTER TABLE paperboat.endpoints
    ADD COLUMN tunnel_id text UNIQUE REFERENCES paperboat.tunnels(id) ON DELETE CASCADE,
    ADD CONSTRAINT endpoints_tunnel_kind CHECK ((kind = 'tunnel') = (tunnel_id IS NOT NULL));

ALTER TABLE paperboat.endpoint_routes
    ADD COLUMN tunnel_route_id text UNIQUE REFERENCES paperboat.tunnel_routes(id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE paperboat.endpoint_routes DROP COLUMN tunnel_route_id;
ALTER TABLE paperboat.endpoints DROP CONSTRAINT endpoints_tunnel_kind, DROP COLUMN tunnel_id;
ALTER TABLE paperboat.control_tunnel_nodes DROP COLUMN tunnel_endpoints_epoch;
ALTER TABLE paperboat.runtime_carrier_attachments
    DROP CONSTRAINT runtime_carrier_attachments_agent_capabilities_check,
    ADD CONSTRAINT runtime_carrier_attachments_agent_capabilities_check
    CHECK (agent_capabilities <@ ARRAY['endpoints']::text[]);
