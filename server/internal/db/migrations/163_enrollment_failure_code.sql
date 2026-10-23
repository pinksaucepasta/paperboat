-- +goose Up
-- A dashboard enrollment refused at approval (the plan's machine limit) moved to failed_retryable with no
-- reason, so the console could only say "failed". failure_code names the refusal (machine_limit_reached);
-- a retry clears it.
ALTER TABLE paperboat.user_machine_pairings ADD COLUMN failure_code text
    CHECK (failure_code IS NULL OR failure_code ~ '^[a-z][a-z0-9_]{0,63}$');

-- +goose Down
ALTER TABLE paperboat.user_machine_pairings DROP COLUMN failure_code;
