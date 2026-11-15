-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE paperboat.team_inbox_requests
 ADD COLUMN stored_metadata jsonb,
 ADD COLUMN stored_prefix text,
 ADD COLUMN stored_uploaded boolean NOT NULL DEFAULT false,
 ADD COLUMN stored_machine_id text,
 ADD COLUMN stored_deleted_at timestamptz,
 ADD COLUMN stored_upload_until timestamptz,
 ADD CONSTRAINT team_inbox_stored_metadata_bound CHECK
 (stored_metadata IS NULL OR (jsonb_typeof(stored_metadata)='object' AND octet_length(stored_metadata::text)<=16777216)) NOT VALID;
-- +goose Down
SET lock_timeout = '5s';
ALTER TABLE paperboat.team_inbox_requests
 DROP COLUMN stored_metadata, DROP COLUMN stored_prefix, DROP COLUMN stored_uploaded,
 DROP COLUMN stored_machine_id, DROP COLUMN stored_deleted_at, DROP COLUMN stored_upload_until;
