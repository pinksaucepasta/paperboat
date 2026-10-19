-- +goose Up
-- The latest origin reachability the host reported for each route of a tunnel. The host reports a
-- state change at once and repeats an unreachable state as a heartbeat; an old report ages out of
-- the tunnel's health instead of staying down forever.
CREATE TABLE paperboat.tunnel_route_origin_health (
    route_id text NOT NULL REFERENCES paperboat.tunnel_routes(id) ON DELETE CASCADE,
    connector_id text NOT NULL REFERENCES paperboat.tunnel_connectors(id) ON DELETE CASCADE,
    tunnel_id text NOT NULL REFERENCES paperboat.tunnels(id) ON DELETE CASCADE,
    session_id text NOT NULL CHECK (length(session_id) BETWEEN 1 AND 128),
    state text NOT NULL CHECK (state IN ('ready', 'unreachable')),
    code text CHECK (code IN ('refused', 'timeout', 'unreachable', 'tls')),
    observed_at timestamptz NOT NULL,
    reported_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (route_id, connector_id),
    CHECK ((state = 'unreachable') = (code IS NOT NULL))
);
CREATE INDEX tunnel_route_origin_health_tunnel ON paperboat.tunnel_route_origin_health (tunnel_id) WHERE state = 'unreachable';

-- +goose Down
DROP TABLE paperboat.tunnel_route_origin_health;
