-- +goose Up
SET LOCAL lock_timeout = '5s';
-- stripe_cancel_at_period_end: an operator's ban of an account or team schedules its subscriptions to end with the paid period
-- (the existing stripe_subscription kind ends them now). The new check is added NOT VALID and validated in the next migration.
ALTER TABLE paperboat.account_deletion_jobs
    DROP CONSTRAINT account_deletion_jobs_kind_check,
    ADD CONSTRAINT account_deletion_jobs_kind_check CHECK (kind IN ('workos_user', 'stripe_subscription', 'stripe_cancel_at_period_end', 'confirmation_mail')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.account_deletion_jobs
    DROP CONSTRAINT account_deletion_jobs_kind_check,
    ADD CONSTRAINT account_deletion_jobs_kind_check CHECK (kind IN ('workos_user', 'stripe_subscription', 'confirmation_mail')) NOT VALID;
