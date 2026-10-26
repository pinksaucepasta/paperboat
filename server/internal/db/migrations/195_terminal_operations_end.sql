-- +goose Up
-- Finished terminal session operations are deleted 7 days after they complete (Lane K terminal plan, slice 8);
-- this index serves that sweep.
-- small-table: one row per terminal lifecycle operation, kept 7 days after it finishes.
CREATE INDEX user_machine_terminal_session_operations_finished
    ON paperboat.user_machine_terminal_session_operations (completed_at)
    WHERE state IN ('applied', 'failed');

-- +goose Down
DROP INDEX paperboat.user_machine_terminal_session_operations_finished;
