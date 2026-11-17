-- +goose NO TRANSACTION
-- +goose Up
-- Operator second factor and the admin audit search, in one migration.
-- TOTP (RFC 6238): the secret is encrypted with the server's encryption key; a row without confirmed_at is an
-- enrolment the operator has not proven yet and grants nothing; last_used_step makes a code usable once.
-- Recovery codes: single-use, only SHA-256 hashes; a used code keeps its row (used_at) for the audit trail.
-- A session created by the password step is pending until the second factor is proven; the API refuses a pending
-- session everywhere except the MFA routes. Existing rows are complete sessions (constant default, no rewrite).
-- The audit search filters the whole append-only log by actor or event type, newest first; the indexes keep each page
-- off a full scan.
SET lock_timeout = '5s';

CREATE TABLE paperboat.operator_totp (
    operator_id text PRIMARY KEY REFERENCES paperboat.operator_accounts(id) ON DELETE CASCADE,
    secret_cipher bytea NOT NULL,
    confirmed_at timestamptz,
    last_used_step bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE paperboat.operator_recovery_codes (
    operator_id text NOT NULL REFERENCES paperboat.operator_accounts(id) ON DELETE CASCADE,
    code_hash bytea NOT NULL CHECK (octet_length(code_hash) = 32),
    used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (operator_id, code_hash)
);

ALTER TABLE paperboat.operator_sessions ADD COLUMN mfa_pending boolean NOT NULL DEFAULT false;

DROP INDEX CONCURRENTLY IF EXISTS paperboat.audit_events_actor_cursor;
CREATE INDEX CONCURRENTLY audit_events_actor_cursor ON paperboat.audit_events (actor_user_id, cursor_sequence DESC) WHERE actor_user_id IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.audit_events_type_cursor;
CREATE INDEX CONCURRENTLY audit_events_type_cursor ON paperboat.audit_events (event_type, cursor_sequence DESC);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.audit_events_actor_cursor;
DROP INDEX CONCURRENTLY IF EXISTS paperboat.audit_events_type_cursor;
ALTER TABLE paperboat.operator_sessions DROP COLUMN mfa_pending;
DROP TABLE paperboat.operator_recovery_codes;
DROP TABLE paperboat.operator_totp;
