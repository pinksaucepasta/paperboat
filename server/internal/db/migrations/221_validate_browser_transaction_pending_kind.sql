-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.browser_access_transactions VALIDATE CONSTRAINT browser_access_transactions_resource_kind_check;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'browser transaction pending kind is forward-only'; END $$;
-- +goose StatementEnd
