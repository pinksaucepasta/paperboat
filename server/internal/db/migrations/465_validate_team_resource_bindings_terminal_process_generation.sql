-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.team_resource_bindings VALIDATE CONSTRAINT team_resource_bindings_terminal_process_generation_check;

-- +goose Down
SELECT 1;
