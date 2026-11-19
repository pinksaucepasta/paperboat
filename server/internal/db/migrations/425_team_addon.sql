-- +goose Up
SET LOCAL lock_timeout = '5s';
-- A team's SSO / SCIM add-on: a second item on the team's Stripe subscription. addon is which one (sso, scim, sso_scim),
-- addon_item_id and addon_price_id the item it is billed on. After the item is removed the add-on keeps working until the paid
-- period ends (addon_until), because removal gives no credit.
ALTER TABLE paperboat.billing_workspaces
    ADD COLUMN addon text,
    ADD COLUMN addon_item_id text,
    ADD COLUMN addon_price_id text,
    ADD COLUMN addon_until timestamptz;
ALTER TABLE paperboat.billing_workspaces
    ADD CONSTRAINT billing_workspaces_addon_check CHECK (addon IS NULL OR addon IN ('sso','scim','sso_scim')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.billing_workspaces
    DROP CONSTRAINT billing_workspaces_addon_check,
    DROP COLUMN addon, DROP COLUMN addon_item_id, DROP COLUMN addon_price_id, DROP COLUMN addon_until;
