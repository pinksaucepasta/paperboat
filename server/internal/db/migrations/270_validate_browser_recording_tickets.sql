-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.browser_terminal_tickets VALIDATE CONSTRAINT browser_terminal_tickets_role_check;
ALTER TABLE paperboat.browser_terminal_tickets VALIDATE CONSTRAINT browser_terminal_ticket_resource;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'browser recording tickets are forward-only'; END $$;
-- +goose StatementEnd
