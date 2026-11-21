-- +goose Up
SET LOCAL lock_timeout = '5s';
-- The gated deploy reports its last outcome (an automatic rollback is an admin alert) and each edge reports its public
-- certificate renewal, one row per edge host; still one row per source, overwritten by each push.
ALTER TABLE paperboat.host_health_reports DROP CONSTRAINT host_health_reports_source_check;
ALTER TABLE paperboat.host_health_reports ADD CONSTRAINT host_health_reports_source_check
    CHECK (source IN ('hosts', 'canary', 'deploy') OR source ~ '^edge-certificate:[A-Za-z0-9.:-]{1,253}$') NOT VALID;

-- +goose Down
SELECT 1;
