-- +goose Up
-- audit_events gets a few thousand rows a day: at the default 0.2 insert and 0.1 analyze scale factors the table
-- (366k rows) was never vacuumed or analyzed, and its planner statistics were 25x stale.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.audit_events SET (autovacuum_vacuum_insert_scale_factor = 0.02, autovacuum_analyze_scale_factor = 0.02, autovacuum_vacuum_scale_factor = 0.05);
ANALYZE paperboat.audit_events;
-- +goose Down
ALTER TABLE paperboat.audit_events RESET (autovacuum_vacuum_insert_scale_factor, autovacuum_analyze_scale_factor, autovacuum_vacuum_scale_factor);
