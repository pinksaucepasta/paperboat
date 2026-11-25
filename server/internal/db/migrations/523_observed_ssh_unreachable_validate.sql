-- +goose Up
-- Every stored set already passed the narrower check.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.user_machines VALIDATE CONSTRAINT user_machines_observed_capabilities_check;

-- +goose Down
SELECT 1;
