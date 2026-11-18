-- +goose NO TRANSACTION
-- +goose Up
-- The operator panel lists billing_workspaces filtered by state, paged by id; without this index each page and its count scan the table.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.billing_workspaces_state_page;
CREATE INDEX CONCURRENTLY billing_workspaces_state_page ON paperboat.billing_workspaces (state, id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.billing_workspaces_state_page;
