-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Operators triage abuse reports: open (the default), actioned (the hostname was taken down) or dismissed, with who
-- did it, when and a note. Constraints are added NOT VALID and validated in 404.
ALTER TABLE paperboat.abuse_reports ADD COLUMN status text NOT NULL DEFAULT 'open';
ALTER TABLE paperboat.abuse_reports ADD COLUMN triaged_by text;
ALTER TABLE paperboat.abuse_reports ADD COLUMN triaged_at timestamptz;
ALTER TABLE paperboat.abuse_reports ADD COLUMN triage_note text NOT NULL DEFAULT '';
ALTER TABLE paperboat.abuse_reports ADD CONSTRAINT abuse_reports_status_check
    CHECK (status IN ('open', 'actioned', 'dismissed')) NOT VALID;
ALTER TABLE paperboat.abuse_reports ADD CONSTRAINT abuse_reports_triage_note_check
    CHECK (char_length(triage_note) <= 1000) NOT VALID;

-- +goose Down
ALTER TABLE paperboat.abuse_reports DROP COLUMN triage_note;
ALTER TABLE paperboat.abuse_reports DROP COLUMN triaged_at;
ALTER TABLE paperboat.abuse_reports DROP COLUMN triaged_by;
ALTER TABLE paperboat.abuse_reports DROP COLUMN status;
