-- +goose Up
ALTER TABLE paperboat.user_machine_terminal_sessions
  ADD COLUMN closed_at timestamp with time zone;

-- Preserve history for sessions that were already observed closed before this
-- migration. Prefer the last runtime observation over later metadata edits.
UPDATE paperboat.user_machine_terminal_sessions
SET closed_at = coalesce(last_runtime_sync_at, updated_at)
WHERE deleted_at IS NULL
  AND runtime_state IN ('closed', 'exited', 'deleted');

CREATE INDEX user_machine_terminal_sessions_retention
  ON paperboat.user_machine_terminal_sessions (closed_at, id)
  WHERE deleted_at IS NULL AND closed_at IS NOT NULL;

-- +goose Down
DROP INDEX paperboat.user_machine_terminal_sessions_retention;
ALTER TABLE paperboat.user_machine_terminal_sessions
  DROP COLUMN closed_at;
