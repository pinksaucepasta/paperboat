-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.tunnel_route_origin_health VALIDATE CONSTRAINT tunnel_route_origin_health_code_check;

-- +goose Down
SELECT 1;
