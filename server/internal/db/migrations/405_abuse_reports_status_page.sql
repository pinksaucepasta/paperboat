-- +goose NO TRANSACTION
-- +goose Up
-- The operator queue lists open reports newest first; this index serves the status filter, its order and its count.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.abuse_reports_status_page;
CREATE INDEX CONCURRENTLY abuse_reports_status_page ON paperboat.abuse_reports (status, created_at DESC, id DESC);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.abuse_reports_status_page;
