-- +goose Up
ALTER TABLE paperboat.teams ADD COLUMN display_name text;
ALTER TABLE paperboat.teams ADD CONSTRAINT team_display_name_valid CHECK (display_name IS NULL OR (octet_length(display_name) BETWEEN 1 AND 128 AND display_name !~ '[[:cntrl:]]'));
