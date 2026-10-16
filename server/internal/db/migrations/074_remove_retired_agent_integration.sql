-- +goose Up
-- Preserve historical migration SQL, but remove the unused integration from
-- current storage. Never silently destroy an unexpected legacy record.
-- +goose StatementBegin
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM paperboat.codex_sessions) THEN
  RAISE EXCEPTION 'Retired integration contains records; archive them before schema cleanup';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE paperboat.codex_sessions;
UPDATE paperboat.user_machines
 SET configured_capabilities=array_remove(configured_capabilities,'codex_host'),
     observed_capabilities=array_remove(observed_capabilities,'codex_host')
 WHERE 'codex_host'=ANY(configured_capabilities) OR 'codex_host'=ANY(observed_capabilities);
ALTER TABLE paperboat.user_machines
 ALTER COLUMN configured_capabilities SET DEFAULT
  ARRAY['file_receive','preview_launch','terminal_host','session_host','ssh_host']::text[],
 DROP CONSTRAINT user_machines_configured_capabilities_check,
 DROP CONSTRAINT user_machines_observed_capabilities_check,
 ADD CONSTRAINT user_machines_configured_capabilities_check CHECK
  (configured_capabilities <@ ARRAY['file_receive','preview_launch','terminal_host','session_host','ssh_host','keep_awake','environment_injection']::text[]),
 ADD CONSTRAINT user_machines_observed_capabilities_check CHECK
  (observed_capabilities <@ ARRAY['file_receive','preview_launch','terminal_host','session_host','ssh_host','keep_awake','environment_injection']::text[]);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 RAISE EXCEPTION 'Removed integration cannot be restored by rollback; restore its protected database backup if required';
END $$;
-- +goose StatementEnd
