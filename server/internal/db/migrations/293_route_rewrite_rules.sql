-- +goose Up
-- A tunnel route can carry header, path-prefix and redirect rules the edge applies (pb tunnel route add --request-header ...):
-- one small JSON document stored with the route; protocol http only, at most 8 KiB of compact JSON (shared/tunnelrouterules
-- owns the shape and the exact bound; jsonb::text adds spaces, so this check is only a coarse 16 KiB guard).
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843); -- tunnel_routes triggers lock shared 443 names
ALTER TABLE paperboat.tunnel_routes ADD COLUMN rewrite_rules jsonb NOT NULL DEFAULT '{}';
ALTER TABLE paperboat.tunnel_routes ADD CONSTRAINT tunnel_routes_rewrite_rules_check
    CHECK (jsonb_typeof(rewrite_rules) = 'object' AND octet_length(rewrite_rules::text) <= 16384
           AND (rewrite_rules = '{}'::jsonb OR protocol = 'http')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.tunnel_routes DROP CONSTRAINT tunnel_routes_rewrite_rules_check;
ALTER TABLE paperboat.tunnel_routes DROP COLUMN rewrite_rules;
