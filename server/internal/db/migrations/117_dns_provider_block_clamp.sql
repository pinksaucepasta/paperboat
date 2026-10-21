-- +goose Up
-- A 429 Retry-After once blocked every write to the preview DNS zone for 94 minutes. The provider's
-- rate-limit window is five minutes; shorten any longer block that is still in force.
UPDATE paperboat.edge_dns_provider_budgets
SET blocked_until = statement_timestamp() + interval '5 minutes'
WHERE blocked_until > statement_timestamp() + interval '5 minutes';

-- +goose Down
SELECT 1;
