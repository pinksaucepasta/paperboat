-- +goose Up
ALTER TABLE paperboat.support_reports
    ADD COLUMN operation text NOT NULL DEFAULT 'http_other',
    ADD COLUMN correlation_id text NOT NULL DEFAULT '',
    ADD COLUMN cause text NOT NULL DEFAULT 'internal',
    ADD COLUMN error_type text NOT NULL DEFAULT 'none',
    ADD COLUMN error_chain text[] NOT NULL DEFAULT '{}',
    ADD COLUMN errno integer NOT NULL DEFAULT 0,
    ADD COLUMN http_status integer NOT NULL DEFAULT 0,
    ADD COLUMN outcome text NOT NULL DEFAULT 'failed',
    ADD COLUMN severity text NOT NULL DEFAULT 'error';

ALTER TABLE paperboat.support_reports
    ADD CONSTRAINT support_reports_fault_operation_check CHECK (operation ~ '^[a-z0-9_]{1,128}$'),
    ADD CONSTRAINT support_reports_fault_correlation_check CHECK (correlation_id = '' OR correlation_id ~ '^[A-Za-z0-9_.:-]{1,200}$'),
    ADD CONSTRAINT support_reports_fault_cause_check CHECK (cause = ANY (ARRAY['none'::text, 'internal'::text, 'context_canceled'::text, 'deadline_exceeded'::text, 'dns_not_found'::text, 'dns_timeout'::text, 'dns_temporary'::text, 'dns_failure'::text, 'tls_unknown_authority'::text, 'tls_hostname'::text, 'tls_verification'::text, 'network_timeout'::text, 'network_unreachable'::text, 'not_found'::text, 'permission_denied'::text, 'conflict'::text, 'rate_limited'::text, 'service_unavailable'::text, 'invalid_request'::text, 'upstream_failure'::text, 'connection_refused'::text, 'connection_reset'::text, 'connection_closed'::text, 'broken_pipe'::text, 'end_of_stream'::text, 'truncated_stream'::text, 'address_in_use'::text, 'invalid_argument'::text, 'resource_exhausted'::text, 'system_error'::text, 'postgres_unique_violation'::text, 'postgres_foreign_key'::text, 'postgres_check_violation'::text, 'postgres_serialization'::text, 'postgres_deadlock'::text, 'postgres_lock_unavailable'::text, 'postgres_query_canceled'::text, 'postgres_connection'::text, 'postgres_authentication'::text, 'postgres_error'::text])),
    ADD CONSTRAINT support_reports_fault_error_type_check CHECK (error_type = 'none' OR error_type ~ '^[A-Za-z0-9_./-]{1,128}$'),
    ADD CONSTRAINT support_reports_fault_errno_check CHECK (errno BETWEEN 0 AND 65535),
    ADD CONSTRAINT support_reports_fault_http_status_check CHECK (http_status = 0 OR http_status BETWEEN 400 AND 599),
    ADD CONSTRAINT support_reports_fault_chain_check CHECK (cardinality(error_chain) <= 8),
    ADD CONSTRAINT support_reports_fault_outcome_check CHECK (outcome = ANY (ARRAY['success'::text, 'failed'::text, 'rejected'::text, 'canceled'::text, 'state_change'::text])),
    ADD CONSTRAINT support_reports_fault_severity_check CHECK (severity = ANY (ARRAY['info'::text, 'warning'::text, 'error'::text]));

-- +goose Down
ALTER TABLE paperboat.support_reports
    DROP CONSTRAINT support_reports_fault_severity_check,
    DROP CONSTRAINT support_reports_fault_outcome_check,
    DROP CONSTRAINT support_reports_fault_chain_check,
    DROP CONSTRAINT support_reports_fault_errno_check,
    DROP CONSTRAINT support_reports_fault_http_status_check,
    DROP CONSTRAINT support_reports_fault_error_type_check,
    DROP CONSTRAINT support_reports_fault_cause_check,
    DROP CONSTRAINT support_reports_fault_correlation_check,
    DROP CONSTRAINT support_reports_fault_operation_check,
    DROP COLUMN severity,
    DROP COLUMN outcome,
    DROP COLUMN errno,
    DROP COLUMN http_status,
    DROP COLUMN error_chain,
    DROP COLUMN error_type,
    DROP COLUMN cause,
    DROP COLUMN correlation_id,
    DROP COLUMN operation;
