-- +goose NO TRANSACTION
-- +goose Up
-- The retention worker deletes previews 30 days after they ended (stopped_at), 500 per statement; without this
-- index each batch scanned every preview ever created.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.preview_leases_ended;
CREATE INDEX CONCURRENTLY preview_leases_ended ON paperboat.preview_leases (stopped_at) WHERE terminal_state <> 'active';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.preview_leases_ended;
