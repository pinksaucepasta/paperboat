-- +goose NO TRANSACTION
-- +goose Up
-- No query filters audit_events by correlation_id: 12 MB (2026-11-25) updated by every insert, never read.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.audit_events_correlation;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.audit_events_correlation;
CREATE INDEX CONCURRENTLY audit_events_correlation ON paperboat.audit_events (correlation_id, cursor_sequence) WHERE correlation_id IS NOT NULL;
