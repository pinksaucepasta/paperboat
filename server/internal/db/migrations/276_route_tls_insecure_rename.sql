-- +goose Up
-- Routes written before 275 named the mode insecure_development.
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843); -- tunnel_routes triggers lock shared 443 names
UPDATE paperboat.tunnel_routes SET tls_verification = 'insecure' WHERE tls_verification = 'insecure_development';

-- +goose Down
SELECT 1;
