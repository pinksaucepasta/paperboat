-- +goose NO TRANSACTION
-- +goose Up
-- The retention worker deletes tunnels, tunnel routes and domains 30 days after their deletion, 500 per statement;
-- these partial indexes keep each batch off a full scan.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnels_deleted;
CREATE INDEX CONCURRENTLY tunnels_deleted ON paperboat.tunnels (deleted_at) WHERE desired_state = 'deleted';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnel_routes_deleted;
CREATE INDEX CONCURRENTLY tunnel_routes_deleted ON paperboat.tunnel_routes (deleted_at) WHERE desired_state = 'deleted';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnel_domains_deleted;
CREATE INDEX CONCURRENTLY tunnel_domains_deleted ON paperboat.tunnel_domains (deleted_at) WHERE deleted_at IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.preview_domains_deleted;
CREATE INDEX CONCURRENTLY preview_domains_deleted ON paperboat.preview_domains (deleted_at) WHERE deleted_at IS NOT NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnels_deleted;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnel_routes_deleted;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.tunnel_domains_deleted;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.preview_domains_deleted;
