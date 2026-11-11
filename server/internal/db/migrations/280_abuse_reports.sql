-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Abuse reports (phishing, malware, ...) about links Paperboat serves, from anyone, no account needed
-- (edge abuse plan 3.6.2). Operators act on them; rows older than 180 days are deleted by the writer.
-- No reporter IP is kept.
CREATE TABLE paperboat.abuse_reports (
    id text PRIMARY KEY CHECK (id ~ '^abuse_[0-9a-f-]{36}$'),
    hostname text NOT NULL CHECK (char_length(hostname) BETWEEN 1 AND 253),
    url text NOT NULL CHECK (char_length(url) BETWEEN 1 AND 2048),
    reason text NOT NULL CHECK (reason IN ('phishing', 'malware', 'spam', 'illegal', 'other')),
    details text NOT NULL DEFAULT '' CHECK (char_length(details) <= 4000),
    reporter_email text NOT NULL DEFAULT '' CHECK (char_length(reporter_email) <= 320),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX abuse_reports_created ON paperboat.abuse_reports(created_at);
CREATE INDEX abuse_reports_hostname ON paperboat.abuse_reports(hostname, created_at DESC);

-- +goose Down
DROP TABLE paperboat.abuse_reports;
