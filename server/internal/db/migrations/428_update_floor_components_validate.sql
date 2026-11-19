-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.client_update_policy VALIDATE CONSTRAINT client_update_policy_singleton_is_pb;

-- +goose Down
SELECT 1;
