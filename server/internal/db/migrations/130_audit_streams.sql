-- +goose Up
-- PLAN F19-10: a team streams its audit log to its SIEM, at most one destination of each kind.
-- Credentials are encrypted with the server key; the cursor is the last delivered event.
CREATE TABLE paperboat.audit_stream_destinations (
 team_id text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('https', 's3')),
 url text NOT NULL CHECK (length(url) BETWEEN 9 AND 2048),
 header_name text CHECK (header_name IS NULL OR length(header_name) <= 64),
 region text CHECK (region IS NULL OR length(region) <= 32),
 bucket text CHECK (bucket IS NULL OR length(bucket) <= 63),
 prefix text CHECK (prefix IS NULL OR length(prefix) <= 256),
 access_key_id text CHECK (access_key_id IS NULL OR length(access_key_id) <= 128),
 secrets bytea NOT NULL,
 cursor_sequence bigint NOT NULL,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 attempts integer NOT NULL DEFAULT 0,
 last_delivered_at timestamptz,
 last_error text CHECK (last_error IS NULL OR length(last_error) <= 200),
 failing_since timestamptz,
 alerted_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (team_id, kind)
);
CREATE INDEX audit_stream_destinations_due ON paperboat.audit_stream_destinations(next_attempt_at);

-- +goose Down
DROP TABLE paperboat.audit_stream_destinations;
