-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.control_config_assignments VALIDATE CONSTRAINT control_config_assignments_source_check;

-- +goose Down
SELECT 1;
