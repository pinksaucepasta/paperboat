-- +goose Up
SET LOCAL lock_timeout = '5s';
-- A recipe machine's pb runs an ordinary CLI session of the recipe's creator that carries the
-- recipe: it ends with the recipe, and every machine authorization of it also checks the reach.
ALTER TABLE paperboat.cli_client_sessions ADD COLUMN recipe_id text;
ALTER TABLE paperboat.cli_client_sessions ADD CONSTRAINT cli_client_sessions_recipe_fk FOREIGN KEY (recipe_id) REFERENCES paperboat.recipes(id) NOT VALID;

-- +goose Down
ALTER TABLE paperboat.cli_client_sessions DROP COLUMN recipe_id;
