-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.billing_workspace_grants VALIDATE CONSTRAINT billing_workspace_grants_kind_check;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'throttle lift grant is forward-only'; END $$;
-- +goose StatementEnd
