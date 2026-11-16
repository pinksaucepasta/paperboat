-- +goose Up
-- A personal workspace records under the scope 'personal:<account>' in the same team_id column the team's
-- recordings use (a team id never holds ':'), so the key chain, copies and index rows need no second table.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.recording_keys DROP CONSTRAINT recording_keys_team_id_fkey;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'personal recording scope is forward-only'; END $$;
-- +goose StatementEnd
