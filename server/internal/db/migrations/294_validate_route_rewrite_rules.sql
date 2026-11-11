-- +goose Up
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843); -- tunnel_routes triggers lock shared 443 names
ALTER TABLE paperboat.tunnel_routes VALIDATE CONSTRAINT tunnel_routes_rewrite_rules_check;

-- +goose Down
SELECT 1;
