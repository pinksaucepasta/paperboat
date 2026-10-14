-- +goose Up
-- Old tickets were issued for the readable carrier and must never be redeemed.
DELETE FROM paperboat.browser_terminal_tickets;
ALTER TABLE paperboat.browser_terminal_tickets
    ADD COLUMN browser_public_key_sha256 text NOT NULL;
ALTER TABLE paperboat.browser_terminal_tickets
    ADD CONSTRAINT browser_terminal_tickets_key_digest_check
    CHECK (length(browser_public_key_sha256) = 43);

-- +goose Down
ALTER TABLE paperboat.browser_terminal_tickets
    DROP CONSTRAINT browser_terminal_tickets_key_digest_check;
ALTER TABLE paperboat.browser_terminal_tickets
    DROP COLUMN browser_public_key_sha256;
