-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.host_health_reports VALIDATE CONSTRAINT host_health_reports_source_check;

-- +goose Down
SELECT 1;
