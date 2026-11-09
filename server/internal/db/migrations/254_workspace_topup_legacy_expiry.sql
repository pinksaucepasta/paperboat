-- +goose Up
-- 249 gave top-ups bought before the expiry existed 'infinity', which the billing overview cannot read
-- (503). Give them a real expiry: 30 days after purchase. Data only, no DDL; one row per purchase.
UPDATE paperboat.billing_workspace_topups SET expires_at = purchased_at + interval '30 days' WHERE expires_at = 'infinity';

-- +goose Down
SELECT 1;
