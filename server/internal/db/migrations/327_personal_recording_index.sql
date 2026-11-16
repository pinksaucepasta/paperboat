-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.session_recordings DROP CONSTRAINT session_recordings_team_id_fkey;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'personal recording scope is forward-only'; END $$;
-- +goose StatementEnd
