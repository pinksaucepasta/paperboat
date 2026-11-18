-- +goose Up
-- POST-DEPLOY: apply one deploy after the code stopped reading control_usage_receipts.quota_exhausted (the receipt queries name their
-- columns without it since the previous deploy). Dropping it in the same deploy as that change would break the servers still reading it.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.control_usage_receipts DROP COLUMN quota_exhausted;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'dropping the usage receipt quota_exhausted column is forward-only'; END $$;
-- +goose StatementEnd
