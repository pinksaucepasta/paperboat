-- +goose Up
SET LOCAL lock_timeout = '5s';
-- A recipe machine's own X25519 public key (made at pairing, private half owner-only on the machine):
-- pb recipe rotate seals the recipe's new private key to it, so running machines keep their vaults.
ALTER TABLE paperboat.recipe_machines ADD COLUMN public_key bytea;

-- +goose Down
ALTER TABLE paperboat.recipe_machines DROP COLUMN public_key;
