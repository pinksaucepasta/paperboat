-- +goose Up
-- DDL only. A route has no stream limit (0) unless its owner sets one.
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843); -- tunnel_routes triggers lock shared 443 names
ALTER TABLE paperboat.tunnel_routes
    ALTER COLUMN max_concurrent_streams SET DEFAULT 0,
    DROP CONSTRAINT tunnel_routes_max_concurrent_streams_check,
    ADD CONSTRAINT tunnel_routes_max_concurrent_streams_check CHECK (max_concurrent_streams BETWEEN 0 AND 100000) NOT VALID;

-- +goose Down
-- Forward only (zero users).
SELECT 1;
