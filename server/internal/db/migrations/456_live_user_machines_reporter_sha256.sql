-- +goose Up
-- live_user_machines is SELECT *, which PostgreSQL expands when the view is created: migration 454's reporter_sha256
-- column was missing from it (TestMachineSummaryCountsTheWholeWorkspaceOnPostgres). The new column is the last one, so
-- CREATE OR REPLACE VIEW can add it.
SET lock_timeout = '5s';

CREATE OR REPLACE VIEW paperboat.live_user_machines AS
 SELECT * FROM paperboat.user_machines WHERE deleted_at IS NULL AND revoked_at IS NULL;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION '456 is forward-only: a view cannot drop a column with CREATE OR REPLACE'; END $$;
-- +goose StatementEnd
