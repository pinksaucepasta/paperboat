-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.operator_actions VALIDATE CONSTRAINT operator_actions_kind_check;

-- +goose Down
SELECT 1;
