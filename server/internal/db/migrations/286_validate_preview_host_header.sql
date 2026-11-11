-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.preview_leases VALIDATE CONSTRAINT preview_leases_host_header_check;

-- +goose Down
SELECT 1;
