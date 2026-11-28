-- +goose Up
-- Every stored state already passed the narrower check.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.transactional_mail_outbox VALIDATE CONSTRAINT transactional_mail_outbox_state_check;

-- +goose Down
SELECT 1;
