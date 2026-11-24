-- +goose Up
-- Existing routes keep their stored limit: the machine serves it from the published snapshot until the route is changed.
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843); -- tunnel_routes triggers lock shared 443 names
ALTER TABLE paperboat.tunnel_routes VALIDATE CONSTRAINT tunnel_routes_max_concurrent_streams_check;

-- +goose Down
SELECT 1;
