-- +goose Up
-- DDL only. 'recorded' ends a mail queued while mail.record_only is on: never handed to the provider, payload kept.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.transactional_mail_outbox DROP CONSTRAINT transactional_mail_outbox_state_check,
 ADD CONSTRAINT transactional_mail_outbox_state_check CHECK
  (state IN ('queued','sent','failed','cancelled','recorded')) NOT VALID;

-- +goose Down
-- Forward only (zero users).
SELECT 1;
