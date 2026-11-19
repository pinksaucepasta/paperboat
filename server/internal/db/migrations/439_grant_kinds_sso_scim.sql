-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Operators can grant a team the SSO or SCIM add-on (amount 1) like any other entitlement.
ALTER TABLE paperboat.billing_workspace_grants DROP CONSTRAINT billing_workspace_grants_kind_check;
ALTER TABLE paperboat.billing_workspace_grants ADD CONSTRAINT billing_workspace_grants_kind_check
    CHECK (kind IN ('bandwidth','throttle_lift','machines','publications','browser_terminals','public_tcp','seats','viewers_per_terminal','sso','scim')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.billing_workspace_grants DROP CONSTRAINT billing_workspace_grants_kind_check;
ALTER TABLE paperboat.billing_workspace_grants ADD CONSTRAINT billing_workspace_grants_kind_check
    CHECK (kind IN ('bandwidth','throttle_lift','machines','publications','browser_terminals','public_tcp','seats','viewers_per_terminal')) NOT VALID;
