-- +goose Up
-- The agent reports an app that redirects visitors to HTTPS on the address they already use.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.tunnel_route_origin_health DROP CONSTRAINT tunnel_route_origin_health_code_check;
ALTER TABLE paperboat.tunnel_route_origin_health ADD CONSTRAINT tunnel_route_origin_health_code_check
    CHECK (code IN ('refused', 'timeout', 'unreachable', 'tls', 'redirect_loop')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.tunnel_route_origin_health DROP CONSTRAINT tunnel_route_origin_health_code_check;
ALTER TABLE paperboat.tunnel_route_origin_health ADD CONSTRAINT tunnel_route_origin_health_code_check
    CHECK (code IN ('refused', 'timeout', 'unreachable', 'tls')) NOT VALID;
