-- +goose Up
-- A preview can serve one Inbox file on its machine (target scheme 'file', address the Inbox file id).
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.preview_leases DROP CONSTRAINT preview_leases_target_scheme_v1_check;
ALTER TABLE paperboat.preview_leases ADD CONSTRAINT preview_leases_target_scheme_v1_check
    CHECK (target_scheme = ANY (ARRAY['http'::text, 'https'::text, 'h2c'::text, 'unix'::text, 'tcp'::text, 'file'::text])) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.preview_leases DROP CONSTRAINT preview_leases_target_scheme_v1_check;
ALTER TABLE paperboat.preview_leases ADD CONSTRAINT preview_leases_target_scheme_v1_check
    CHECK (target_scheme = ANY (ARRAY['http'::text, 'https'::text, 'h2c'::text, 'unix'::text, 'tcp'::text])) NOT VALID;
