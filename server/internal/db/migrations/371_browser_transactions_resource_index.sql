-- +goose NO TRANSACTION
-- +goose Up
-- Begin counts open sign-in transactions per resource under a per-resource lock (it was one server-wide lock and cap).
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.browser_access_transactions_resource;
CREATE INDEX CONCURRENTLY browser_access_transactions_resource
 ON paperboat.browser_access_transactions (resource_kind, resource_id, expires_at);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.browser_access_transactions_resource;
