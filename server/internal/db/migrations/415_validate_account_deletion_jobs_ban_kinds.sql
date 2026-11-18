-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.account_deletion_jobs VALIDATE CONSTRAINT account_deletion_jobs_kind_check;

-- +goose Down
SELECT 1;
