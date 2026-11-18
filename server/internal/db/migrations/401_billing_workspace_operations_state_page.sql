-- +goose NO TRANSACTION
-- +goose Up
-- The operator panel lists billing_workspace_operations filtered by state, paged by idempotency_key; without this index each page and its count scan the table.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.billing_workspace_operations_state_page;
CREATE INDEX CONCURRENTLY billing_workspace_operations_state_page ON paperboat.billing_workspace_operations (state, idempotency_key);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.billing_workspace_operations_state_page;
