-- +goose Up
-- DDL only. A machine whose SSH server stopped answering reports the observed capability ssh_unreachable; an unknown
-- name would refuse its whole runtime observation, so the server accepts it before any machine sends it.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.user_machines DROP CONSTRAINT user_machines_observed_capabilities_check,
 ADD CONSTRAINT user_machines_observed_capabilities_check CHECK
  (observed_capabilities <@ ARRAY['file_receive','preview_launch','terminal_host','session_host','ssh_host','keep_awake','environment_injection','file_transfer_modes','ssh_unreachable']::text[]) NOT VALID;

-- +goose Down
-- Forward only (zero users).
SELECT 1;
