-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
-- Runtime attach synchronizes tunnels and removes obsolete assignments for one
-- machine. Those lookups include removed endpoints, outside endpoints_agent.
DROP INDEX CONCURRENTLY IF EXISTS paperboat.endpoints_by_agent;
CREATE INDEX CONCURRENTLY endpoints_by_agent ON paperboat.endpoints(agent_machine_id);

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.endpoints_by_agent;
