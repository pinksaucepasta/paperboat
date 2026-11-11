-- +goose Up
-- Routes written before 271 named the mode insecure_development.
UPDATE paperboat.tunnel_routes SET tls_verification = 'insecure' WHERE tls_verification = 'insecure_development';

-- +goose Down
SELECT 1;
