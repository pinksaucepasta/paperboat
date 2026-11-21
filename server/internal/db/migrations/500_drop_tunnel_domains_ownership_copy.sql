-- +goose Up
-- contract-after: 498
-- DDL only. workspace_domains owns verification (497); since 9de7da467 no server code names these columns
-- (db/sqlc_ignored_columns.sql), so servers of the previous build keep working while this runs.
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.tunnel_domains DROP COLUMN ownership_state, DROP COLUMN last_verified_at;

-- +goose Down
-- Forward only (zero users): the dropped copy is not rebuilt.
SELECT 1;
