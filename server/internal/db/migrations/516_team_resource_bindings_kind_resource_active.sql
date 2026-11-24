-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
-- Sharing lookups know the resource, not the team, which the primary key leads with.
-- A CONCURRENTLY build that was interrupted leaves an invalid index of this name; drop it first.
DROP INDEX CONCURRENTLY IF EXISTS paperboat.team_resource_bindings_kind_resource_active;
CREATE INDEX CONCURRENTLY IF NOT EXISTS team_resource_bindings_kind_resource_active
  ON paperboat.team_resource_bindings (resource_kind, resource_id) INCLUDE (team_id, generation)
  WHERE active;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.team_resource_bindings_kind_resource_active;
