-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.preview_leases_active_by_owner;
CREATE INDEX CONCURRENTLY preview_leases_active_by_owner
    ON paperboat.preview_leases(owner_machine_id, lease_deadline) WHERE terminal_state='active';

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.preview_leases_active_by_owner;
