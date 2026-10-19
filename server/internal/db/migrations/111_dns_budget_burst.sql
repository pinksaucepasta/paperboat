-- +goose Up
-- The burst of the DNS provider budget is owned by the server (tunnelcert), not by a table limit.
ALTER TABLE paperboat.edge_dns_provider_budgets DROP CONSTRAINT edge_dns_provider_budgets_tokens_check;
ALTER TABLE paperboat.edge_dns_provider_budgets ADD CONSTRAINT edge_dns_provider_budgets_tokens_check CHECK (tokens >= 0);

-- +goose Down
UPDATE paperboat.edge_dns_provider_budgets SET tokens = LEAST(tokens, 5);
ALTER TABLE paperboat.edge_dns_provider_budgets DROP CONSTRAINT edge_dns_provider_budgets_tokens_check;
ALTER TABLE paperboat.edge_dns_provider_budgets ADD CONSTRAINT edge_dns_provider_budgets_tokens_check CHECK (tokens >= 0 AND tokens <= 5);
