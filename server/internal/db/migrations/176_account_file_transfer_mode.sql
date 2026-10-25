-- +goose Up
-- The account's default file transfer mode (PLAN 3b File transfer modes): `pb send` and the console
-- use it unless a send names one. Automatic is bounded and may use Paperboat-hosted relays;
-- Unlimited is unbounded and never does.
ALTER TABLE paperboat.users ADD COLUMN file_transfer_mode text NOT NULL DEFAULT 'automatic'
    CHECK (file_transfer_mode IN ('automatic', 'unlimited'));

-- +goose Down
ALTER TABLE paperboat.users DROP COLUMN file_transfer_mode;
