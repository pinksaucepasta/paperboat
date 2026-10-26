-- +goose Up
-- F21-08: mint signing keys withdrawn while the server runs. A row is written by
-- `paperboat-server admin mint-key withdraw`, which notifies paperboat_mint_withdrawn in the same
-- transaction; every server process then refuses the key at once and publishes it in
-- withdrawn_key_ids, without a restart. Rows are never deleted: a withdrawn key stays withdrawn.
-- small-table: one row per withdrawn signing key (a handful over the deployment's life).
CREATE TABLE paperboat.mint_withdrawn_keys (
    key_id text PRIMARY KEY CHECK (key_id <> '' AND length(key_id) <= 128),
    actor_user_id text NOT NULL CHECK (actor_user_id <> ''),
    reason text NOT NULL CHECK (reason <> '' AND length(reason) <= 500),
    withdrawn_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE paperboat.mint_withdrawn_keys;
