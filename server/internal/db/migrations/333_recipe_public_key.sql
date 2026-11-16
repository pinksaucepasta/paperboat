-- +goose Up
SET LOCAL lock_timeout = '5s';
-- A recipe's X25519 public key (shared/recipe Token.PublicKey; not secret): pb recipe update and the
-- console wrap a vault key a recipe newly reads to it without the token.
ALTER TABLE paperboat.recipes ADD COLUMN public_key bytea;

-- +goose Down
ALTER TABLE paperboat.recipes DROP COLUMN public_key;
