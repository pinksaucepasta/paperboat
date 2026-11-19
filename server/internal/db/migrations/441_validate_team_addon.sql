-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.billing_workspaces VALIDATE CONSTRAINT billing_workspaces_addon_check;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'team add-on check is forward-only'; END $$;
-- +goose StatementEnd
