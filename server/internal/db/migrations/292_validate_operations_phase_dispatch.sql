-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.operations VALIDATE CONSTRAINT operations_phase_check;

-- +goose Down
SELECT 1;
