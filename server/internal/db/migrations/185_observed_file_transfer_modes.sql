-- +goose Up
-- Destinations that receive per the signed file transfer claims (PLAN 3b File transfer modes; pb release N+1)
-- report the observed capability file_transfer_modes. It is accepted before any client sends it: an unknown
-- name would refuse every runtime observation from those machines.
ALTER TABLE paperboat.user_machines DROP CONSTRAINT user_machines_observed_capabilities_check,
 ADD CONSTRAINT user_machines_observed_capabilities_check CHECK
  (observed_capabilities <@ ARRAY['file_receive','preview_launch','terminal_host','session_host','ssh_host','keep_awake','environment_injection','file_transfer_modes']::text[]);

-- +goose Down
UPDATE paperboat.user_machines SET observed_capabilities=array_remove(observed_capabilities,'file_transfer_modes');
ALTER TABLE paperboat.user_machines DROP CONSTRAINT user_machines_observed_capabilities_check,
 ADD CONSTRAINT user_machines_observed_capabilities_check CHECK
  (observed_capabilities <@ ARRAY['file_receive','preview_launch','terminal_host','session_host','ssh_host','keep_awake','environment_injection']::text[]);
