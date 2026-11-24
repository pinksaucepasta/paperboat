-- +goose Up
-- contract-after: 447
-- DDL only. 447 made the column optional and the server stopped writing or reading it (one HuJSON file format).
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.recipes DROP COLUMN source_format;

-- +goose Down
-- Forward only (zero users): the dropped column is not rebuilt.
SELECT 1;
