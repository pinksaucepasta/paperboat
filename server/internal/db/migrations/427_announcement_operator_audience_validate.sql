-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.announcements VALIDATE CONSTRAINT announcements_audience_kind_check;
ALTER TABLE paperboat.announcements VALIDATE CONSTRAINT announcements_audience_value_check;

-- +goose Down
SELECT 1;
