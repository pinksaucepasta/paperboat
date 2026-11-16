-- +goose Up
SET LOCAL lock_timeout = '5s';
-- A policy version saved by a recipe (machine tags at pairing) names the recipe as its author.
ALTER TABLE paperboat.workspace_policies ADD COLUMN updated_with_recipe text;
ALTER TABLE paperboat.workspace_policy_versions ADD COLUMN recipe text;

-- +goose Down
ALTER TABLE paperboat.workspace_policy_versions DROP COLUMN recipe;
ALTER TABLE paperboat.workspace_policies DROP COLUMN updated_with_recipe;
