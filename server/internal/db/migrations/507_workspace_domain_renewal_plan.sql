-- +goose Up
-- The renewal time of each workspace domain's certificate (the CA's suggestion, else the shared fallback) and when the
-- CA is asked again. NULL until the worker has planned the certificate.
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.workspace_domains ADD COLUMN certificate_renewal_at timestamptz, ADD COLUMN certificate_renewal_check_at timestamptz;
CREATE INDEX workspace_domains_renewal_check ON paperboat.workspace_domains (certificate_renewal_check_at NULLS FIRST, id)
    WHERE ownership_state = 'verified';

-- +goose Down
ALTER TABLE paperboat.workspace_domains DROP COLUMN certificate_renewal_at, DROP COLUMN certificate_renewal_check_at;
