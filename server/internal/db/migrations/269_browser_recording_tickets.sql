-- +goose Up
-- Session recording replay in the browser: the same single-use browser ticket as a terminal or inspector channel,
-- bound to one recording (role 'recording', an account principal, no terminal session). The constraints are
-- replaced NOT VALID and validated in 270.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.browser_terminal_tickets ADD COLUMN recording_id text;
ALTER TABLE paperboat.browser_terminal_tickets DROP CONSTRAINT browser_terminal_tickets_role_check;
ALTER TABLE paperboat.browser_terminal_tickets ADD CONSTRAINT browser_terminal_tickets_role_check
    CHECK (role IN ('owner', 'viewer', 'interactive', 'inspector', 'recording')) NOT VALID;
ALTER TABLE paperboat.browser_terminal_tickets DROP CONSTRAINT browser_terminal_ticket_resource;
ALTER TABLE paperboat.browser_terminal_tickets ADD CONSTRAINT browser_terminal_ticket_resource CHECK (
    (role NOT IN ('inspector', 'recording') AND terminal_session_id IS NOT NULL AND inspector_credential_id IS NULL AND recording_id IS NULL)
    OR (role = 'inspector' AND terminal_session_id IS NULL AND inspector_credential_id IS NOT NULL AND guest_session_id IS NULL AND recording_id IS NULL)
    OR (role = 'recording' AND terminal_session_id IS NULL AND inspector_credential_id IS NULL AND guest_session_id IS NULL AND recording_id IS NOT NULL)
) NOT VALID;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'browser recording tickets are forward-only'; END $$;
-- +goose StatementEnd
