-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.host_health_reports DROP CONSTRAINT host_health_reports_source_check;
ALTER TABLE paperboat.host_health_reports ADD CONSTRAINT host_health_reports_source_check CHECK (source IN ('hosts', 'canary', 'deploy')) NOT VALID;

-- +goose Down
SELECT 1;
