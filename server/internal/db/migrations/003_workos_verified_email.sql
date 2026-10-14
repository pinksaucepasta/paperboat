-- +goose Up
ALTER TABLE paperboat.users ADD COLUMN email_verified boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE paperboat.users DROP COLUMN email_verified;
