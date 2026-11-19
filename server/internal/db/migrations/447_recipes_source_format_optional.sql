-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Recipe files are one format (HuJSON), so the server no longer writes source_format. The column is
-- dropped one deploy later, once no running server reads it.
ALTER TABLE paperboat.recipes ALTER COLUMN source_format DROP NOT NULL;
ALTER TABLE paperboat.recipes DROP CONSTRAINT IF EXISTS recipes_source_format_check;

-- +goose Down
SELECT 1;
