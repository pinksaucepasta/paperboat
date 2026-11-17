-- +goose NO TRANSACTION
-- +goose Up
-- The retention worker deletes replay records, finished reconciliation attempts and withdrawn DNS publications, 500
-- per statement; these indexes keep each batch off a full scan.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_route_operations_created;
CREATE INDEX CONCURRENTLY control_route_operations_created ON paperboat.control_route_operations (created_at);
DROP INDEX CONCURRENTLY IF EXISTS paperboat.environment_vault_operations_created;
CREATE INDEX CONCURRENTLY environment_vault_operations_created ON paperboat.environment_vault_operations (created_at);
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_capability_operations_created;
CREATE INDEX CONCURRENTLY user_machine_capability_operations_created ON paperboat.user_machine_capability_operations (created_at);
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_reconciliation_attempts_finished;
CREATE INDEX CONCURRENTLY control_reconciliation_attempts_finished ON paperboat.control_reconciliation_attempts (finished_at)
    WHERE state IN ('succeeded', 'failed', 'cancelled');
DROP INDEX CONCURRENTLY IF EXISTS paperboat.edge_dns_publications_withdrawn;
CREATE INDEX CONCURRENTLY edge_dns_publications_withdrawn ON paperboat.edge_dns_publications (updated_at)
    WHERE state = 'verified' AND desired_addresses = '[]'::jsonb AND provider_records = '[]'::jsonb;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.edge_dns_publications_withdrawn;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_reconciliation_attempts_finished;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.user_machine_capability_operations_created;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.environment_vault_operations_created;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.control_route_operations_created;
