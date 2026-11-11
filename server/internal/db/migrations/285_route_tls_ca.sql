-- +goose Up
-- A custom_ca route stores the PEM CA it trusts for its https origin (cloudflared's caPool); it replaces
-- ca_reference, which named a secret-store entry nothing could create. ca_reference is dropped one deploy
-- after the code stopped reading it.
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.tunnel_routes ADD COLUMN tls_ca text;
ALTER TABLE paperboat.tunnel_routes ADD CONSTRAINT tunnel_routes_tls_ca_check
    CHECK (tls_ca IS NULL OR (tls_verification = 'custom_ca' AND length(tls_ca) <= 16384)) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.tunnel_routes DROP CONSTRAINT tunnel_routes_tls_ca_check;
ALTER TABLE paperboat.tunnel_routes DROP COLUMN tls_ca;
