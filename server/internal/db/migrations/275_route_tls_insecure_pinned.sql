-- +goose Up
-- Origin TLS verification: insecure replaces insecure_development (allowed in production), and pinned
-- trusts exactly one certificate by its SHA-256 fingerprint (tls_pin).
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.tunnel_routes ADD COLUMN tls_pin text;
ALTER TABLE paperboat.tunnel_routes DROP CONSTRAINT tunnel_routes_tls_verification_check;
ALTER TABLE paperboat.tunnel_routes ADD CONSTRAINT tunnel_routes_tls_verification_check
    CHECK (tls_verification = ANY (ARRAY['not_applicable'::text, 'system'::text, 'custom_ca'::text, 'mutual_tls'::text, 'insecure'::text, 'pinned'::text])) NOT VALID;
ALTER TABLE paperboat.tunnel_routes ADD CONSTRAINT tunnel_routes_tls_pin_check
    CHECK ((tls_verification = 'pinned') = (tls_pin IS NOT NULL) AND (tls_pin IS NULL OR tls_pin ~ '^sha256:[0-9a-f]{64}$')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.tunnel_routes DROP CONSTRAINT tunnel_routes_tls_pin_check;
ALTER TABLE paperboat.tunnel_routes DROP CONSTRAINT tunnel_routes_tls_verification_check;
ALTER TABLE paperboat.tunnel_routes ADD CONSTRAINT tunnel_routes_tls_verification_check
    CHECK (tls_verification = ANY (ARRAY['not_applicable'::text, 'system'::text, 'custom_ca'::text, 'mutual_tls'::text, 'insecure_development'::text])) NOT VALID;
ALTER TABLE paperboat.tunnel_routes DROP COLUMN tls_pin;
