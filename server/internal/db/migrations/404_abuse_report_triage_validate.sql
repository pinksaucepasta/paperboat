-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.abuse_reports VALIDATE CONSTRAINT abuse_reports_status_check;
ALTER TABLE paperboat.abuse_reports VALIDATE CONSTRAINT abuse_reports_triage_note_check;

-- +goose Down
SELECT 1;
