-- +goose Up
-- A machine has a name and tags; the free-text description is gone (user decision 2026-10-23:
-- tags take its place). No function, view or trigger reads either column.
ALTER TABLE paperboat.user_machines DROP COLUMN description;
ALTER TABLE paperboat.user_machine_pairings DROP COLUMN requested_description;

-- +goose Down
ALTER TABLE paperboat.user_machine_pairings ADD COLUMN requested_description text NOT NULL DEFAULT '';
ALTER TABLE paperboat.user_machines ADD COLUMN description text NOT NULL DEFAULT '';
