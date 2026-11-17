-- +goose Up
-- One support_reports row per error occurrence became ~6.4k/day from 25 machines and ~2.6M/day at 10k. Occurrences of
-- one failure share everything but their ids and times, so that is stored once per account and fingerprint (a group:
-- first/last seen, count, the fault's detail) beside a narrow row per occurrence that keeps every shown reference
-- resolvable. The server reads and writes only these two tables from this migration on; support_reports is dropped in
-- the next deploy's migration, after the previous server has stopped reading it.
SET lock_timeout = '5s';

-- Component, operation, stage, code, cause, error type, status, outcome and source line name one failure; the
-- release, errno and chain do not (Sentry groups across releases the same way). Owned here so the writer and the
-- backfill cannot disagree.
CREATE FUNCTION paperboat.support_fingerprint(component text, operation text, stage text, code text, cause text,
  error_type text, http_status integer, outcome text, source_file text, source_function text, source_line integer)
RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT encode(sha256(convert_to(concat_ws(chr(31), component, operation, stage, code, cause, error_type,
    http_status::text, outcome, source_file, source_function, source_line::text), 'UTF8')), 'hex')
$$;

CREATE TABLE paperboat.support_report_groups (
    owner_id text NOT NULL,
    fingerprint text NOT NULL,
    component text NOT NULL,
    operation text NOT NULL,
    stage text NOT NULL,
    code text NOT NULL,
    cause text NOT NULL,
    error_type text NOT NULL,
    http_status integer NOT NULL,
    outcome text NOT NULL,
    source_file text NOT NULL,
    source_function text NOT NULL,
    source_line integer NOT NULL,
    severity text NOT NULL,
    -- From the newest occurrence: they can differ between occurrences of one fingerprint.
    errno integer NOT NULL,
    error_chain text[] NOT NULL DEFAULT '{}',
    first_seen timestamp with time zone NOT NULL,
    last_seen timestamp with time zone NOT NULL,
    occurrences bigint NOT NULL CHECK (occurrences > 0),
    -- The newest occurrence's expiry: the group outlives every occurrence row it explains.
    expires_at timestamp with time zone NOT NULL,
    PRIMARY KEY (owner_id, fingerprint),
    CONSTRAINT support_report_groups_fault_check CHECK (
      operation ~ '^[a-z0-9_]{1,128}$' AND errno BETWEEN 0 AND 65535 AND (http_status = 0 OR http_status BETWEEN 400 AND 599)
      AND cardinality(error_chain) <= 8
      AND outcome = ANY (ARRAY['success','failed','rejected','canceled','state_change']))
);
CREATE INDEX support_report_groups_expires ON paperboat.support_report_groups (expires_at);

CREATE TABLE paperboat.support_report_occurrences (
    id text PRIMARY KEY,
    owner_id text NOT NULL,
    support_reference text NOT NULL CHECK (support_reference ~ '^support_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    fingerprint text NOT NULL,
    request_id text NOT NULL,
    correlation_id text NOT NULL CHECK (correlation_id = '' OR correlation_id ~ '^[A-Za-z0-9_.:-]{1,200}$'),
    release text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    sentry_event_id text NOT NULL,
    reporting text NOT NULL CHECK (reporting = ANY (ARRAY['disabled','filtered','unconfirmed']))
);
-- The reference lookup, the group's recent sample, the per-account cap and the expiry batches.
CREATE INDEX support_report_occurrences_reference ON paperboat.support_report_occurrences (support_reference, owner_id);
CREATE INDEX support_report_occurrences_group ON paperboat.support_report_occurrences (owner_id, fingerprint, created_at DESC);
CREATE INDEX support_report_occurrences_expires ON paperboat.support_report_occurrences (expires_at);

-- +goose Down
DROP TABLE IF EXISTS paperboat.support_report_occurrences;
DROP TABLE IF EXISTS paperboat.support_report_groups;
DROP FUNCTION IF EXISTS paperboat.support_fingerprint;
