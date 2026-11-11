-- +goose Up
SET LOCAL lock_timeout = '5s';
-- How many of a recording's leading segments the machine has put in the team bucket (0 when it has no bucket).
-- Application code keeps it between 0 and segments; the column has a constant default, so adding it rewrites nothing.
ALTER TABLE paperboat.session_recordings ADD COLUMN archived_segments bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE paperboat.session_recordings DROP COLUMN archived_segments;
