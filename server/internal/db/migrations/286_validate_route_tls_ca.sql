-- +goose Up
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.tunnel_routes VALIDATE CONSTRAINT tunnel_routes_tls_ca_check;

-- +goose Down
SELECT 1;
