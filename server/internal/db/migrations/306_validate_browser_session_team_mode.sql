-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.browser_access_sessions VALIDATE CONSTRAINT browser_access_sessions_team_mode_check;

-- +goose Down
SELECT 1;
