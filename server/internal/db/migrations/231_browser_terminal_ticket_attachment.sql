-- +goose Up
-- An edge ticket names the attachment it admitted. The edge resumes that attachment's control
-- channel when a server rollout's proxy closes it (Caddy stream_close_delay), and ticket cleanup
-- keeps a consumed ticket while its attachment is still attached.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.browser_terminal_tickets ADD COLUMN edge_attachment_id text;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.browser_terminal_tickets DROP COLUMN edge_attachment_id;
