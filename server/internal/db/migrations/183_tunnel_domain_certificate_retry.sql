-- +goose Up
-- A durable-route certificate that failed waits before its next issuance attempt. The worker computes
-- the retry time from the consecutive failure count (one minute, doubling, capped at an hour, or the
-- issuer's Retry-After), so the bounds live in one place: tunnelv1's certificate retry policy. Both
-- columns are cleared when the certificate becomes ready.
ALTER TABLE paperboat.tunnel_domains
  ADD COLUMN certificate_retry_at timestamptz,
  ADD COLUMN certificate_failure_attempts integer NOT NULL DEFAULT 0 CHECK (certificate_failure_attempts >= 0);

-- +goose Down
ALTER TABLE paperboat.tunnel_domains DROP COLUMN certificate_failure_attempts, DROP COLUMN certificate_retry_at;
