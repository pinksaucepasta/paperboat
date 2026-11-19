-- +goose Up
-- Post-deploy: no server reads or writes this column since 2026.11.19.5 (1d2924bb4); dropped one deploy later.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.control_usage_receipts DROP COLUMN quota_exhausted;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'dropping control_usage_receipts.quota_exhausted is forward-only'; END $$;
-- +goose StatementEnd
