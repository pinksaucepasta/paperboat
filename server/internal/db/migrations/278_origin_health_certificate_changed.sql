-- +goose Up
-- The agent reports a pinned route whose origin presents a different certificate.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.tunnel_route_origin_health DROP CONSTRAINT tunnel_route_origin_health_code_check;
ALTER TABLE paperboat.tunnel_route_origin_health ADD CONSTRAINT tunnel_route_origin_health_code_check
    CHECK (code IN ('refused', 'timeout', 'unreachable', 'tls', 'redirect_loop', 'certificate_changed')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.tunnel_route_origin_health DROP CONSTRAINT tunnel_route_origin_health_code_check;
ALTER TABLE paperboat.tunnel_route_origin_health ADD CONSTRAINT tunnel_route_origin_health_code_check
    CHECK (code IN ('refused', 'timeout', 'unreachable', 'tls', 'redirect_loop')) NOT VALID;
