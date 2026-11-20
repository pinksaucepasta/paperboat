-- +goose NO TRANSACTION
-- +goose Up
-- Retention deletes edge usage receipts by age (created_at). Nothing ever set acknowledged_at, which the old policy
-- waited for, so receipts grew without bound (311,028 rows, 237 MB in production 2026-11-20); a batch reads the oldest
-- rows from this index and never scans the table.
SET lock_timeout = '5s';
-- A failed concurrent build leaves an INVALID index that IF NOT EXISTS would keep: drop any leftover, then build.
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_usage_receipts_created;
CREATE INDEX CONCURRENTLY control_usage_receipts_created ON paperboat.control_usage_receipts (created_at);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_usage_receipts_created;
