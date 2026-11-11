-- +goose Up
-- A preview can name the Host header its origin receives (pb preview --host-header): http, https and h2c targets only.
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.preview_leases ADD COLUMN host_header text;
ALTER TABLE paperboat.preview_leases ADD CONSTRAINT preview_leases_host_header_check
    CHECK (host_header IS NULL OR (length(host_header) BETWEEN 1 AND 253 AND target_scheme IN ('http', 'https', 'h2c'))) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.preview_leases DROP CONSTRAINT preview_leases_host_header_check;
ALTER TABLE paperboat.preview_leases DROP COLUMN host_header;
