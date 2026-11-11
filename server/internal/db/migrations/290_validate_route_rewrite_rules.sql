-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.tunnel_routes VALIDATE CONSTRAINT tunnel_routes_rewrite_rules_check;

-- +goose Down
SELECT 1;
