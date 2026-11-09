-- +goose Up
-- A top-up belongs to the allowance month it was bought in and is gone when that month ends.
-- Expand only: rows that exist before this migration keep 'infinity' (they were sold as permanent);
-- every new row sets expires_at itself, so the default is dropped at once.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.billing_workspace_topups ADD COLUMN expires_at timestamptz NOT NULL DEFAULT 'infinity';
ALTER TABLE paperboat.billing_workspace_topups ALTER COLUMN expires_at DROP DEFAULT;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.billing_workspace_topups DROP COLUMN expires_at;
