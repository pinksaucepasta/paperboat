-- +goose Up
ALTER TABLE paperboat.user_machine_terminal_session_operations
  DROP CONSTRAINT user_machine_terminal_session_operations_operation_check;

ALTER TABLE paperboat.user_machine_terminal_session_operations
  ADD CONSTRAINT user_machine_terminal_session_operations_operation_check
  CHECK (operation = ANY (ARRAY['create'::text, 'close'::text, 'delete_history'::text]));
