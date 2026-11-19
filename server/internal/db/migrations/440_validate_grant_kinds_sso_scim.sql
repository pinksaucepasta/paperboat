-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.billing_workspace_grants VALIDATE CONSTRAINT billing_workspace_grants_kind_check;

-- +goose Down
SELECT 1;
