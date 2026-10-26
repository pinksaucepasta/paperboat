-- +goose Up
-- Machine limits come from billing.limits in the server configuration; nothing reads or writes this column.
ALTER TABLE paperboat.billing_operator_policy DROP COLUMN personal_machine_limit;

-- +goose Down
ALTER TABLE paperboat.billing_operator_policy ADD COLUMN personal_machine_limit integer
    CONSTRAINT billing_operator_policy_personal_machine_limit_check CHECK (personal_machine_limit IS NULL OR personal_machine_limit >= 0);
