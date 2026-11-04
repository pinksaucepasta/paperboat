-- +goose Up
-- A signed-in account without access now gets a handoff marked denied, so the edge itself answers 403.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.browser_access_transactions ADD COLUMN denied boolean NOT NULL DEFAULT false;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'browser transaction denied column is forward-only'; END $$;
-- +goose StatementEnd
