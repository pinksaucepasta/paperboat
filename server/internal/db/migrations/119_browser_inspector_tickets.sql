-- +goose Up
-- F17: a console inspector channel is admitted by the same single-use browser
-- ticket as a terminal, bound to one inspector credential instead of a
-- terminal session. Role 'inspector' marks it; it never has a guest principal.
ALTER TABLE paperboat.browser_terminal_tickets ALTER COLUMN terminal_session_id DROP NOT NULL;
ALTER TABLE paperboat.browser_terminal_tickets
    ADD COLUMN inspector_credential_id text REFERENCES paperboat.inspector_credentials(credential_id) ON DELETE CASCADE;
ALTER TABLE paperboat.browser_terminal_tickets DROP CONSTRAINT browser_terminal_tickets_role_check;
ALTER TABLE paperboat.browser_terminal_tickets ADD CONSTRAINT browser_terminal_tickets_role_check
    CHECK (role IN ('owner', 'viewer', 'interactive', 'inspector'));
ALTER TABLE paperboat.browser_terminal_tickets ADD CONSTRAINT browser_terminal_ticket_resource CHECK (
    (role <> 'inspector' AND terminal_session_id IS NOT NULL AND inspector_credential_id IS NULL)
    OR (role = 'inspector' AND terminal_session_id IS NULL AND inspector_credential_id IS NOT NULL AND guest_session_id IS NULL)
);

-- +goose Down
DELETE FROM paperboat.browser_terminal_tickets WHERE role = 'inspector';
ALTER TABLE paperboat.browser_terminal_tickets DROP CONSTRAINT browser_terminal_ticket_resource;
ALTER TABLE paperboat.browser_terminal_tickets DROP CONSTRAINT browser_terminal_tickets_role_check;
ALTER TABLE paperboat.browser_terminal_tickets ADD CONSTRAINT browser_terminal_tickets_role_check
    CHECK (role IN ('owner', 'viewer', 'interactive'));
ALTER TABLE paperboat.browser_terminal_tickets DROP COLUMN inspector_credential_id;
ALTER TABLE paperboat.browser_terminal_tickets ALTER COLUMN terminal_session_id SET NOT NULL;
