-- +goose NO TRANSACTION
-- +goose Up
-- The operator panel lists control_operations filtered by state, paged by id; without this index each page and its count scan the table.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_operations_state_page;
CREATE INDEX CONCURRENTLY control_operations_state_page ON paperboat.control_operations (state, id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_operations_state_page;
