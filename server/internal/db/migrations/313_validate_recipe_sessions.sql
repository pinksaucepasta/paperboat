-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.cli_client_sessions VALIDATE CONSTRAINT cli_client_sessions_recipe_fk;
CREATE INDEX cli_client_sessions_recipe ON paperboat.cli_client_sessions (recipe_id) WHERE recipe_id IS NOT NULL;

-- +goose Down
DROP INDEX paperboat.cli_client_sessions_recipe;
