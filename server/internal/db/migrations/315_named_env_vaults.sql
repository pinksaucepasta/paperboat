-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Named ENV vaults (PLAN F26): each has its own 32-byte key, made on a member's computer; the
-- values are one envelope encrypted with the key of key_epoch. The server stores only ciphertext
-- and wraps: per member or team (environment_named_vault_wraps) and per recipe (recipe_vault_wraps,
-- to the recipe's public key). rekey_required is set when a recipe that could read the vault is
-- revoked; new values wait for a new key epoch.
CREATE TABLE paperboat.environment_named_vaults (
    id text PRIMARY KEY,
    owner_account text NOT NULL REFERENCES paperboat.users(id),
    team_id text,
    name text NOT NULL,
    key_epoch integer NOT NULL DEFAULT 1 CHECK (key_epoch >= 1),
    revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
    envelope bytea CHECK (octet_length(envelope) <= 1048576),
    rekey_required boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);
CREATE UNIQUE INDEX environment_named_vaults_live_name ON paperboat.environment_named_vaults (coalesce(team_id, 'personal:' || owner_account), name) WHERE deleted_at IS NULL;

CREATE TABLE paperboat.environment_named_vault_wraps (
    vault_id text NOT NULL REFERENCES paperboat.environment_named_vaults(id) ON DELETE CASCADE,
    recipient_kind text NOT NULL CHECK (recipient_kind IN ('account', 'team')),
    recipient_id text NOT NULL,
    key_epoch integer NOT NULL,
    wrapped bytea NOT NULL CHECK (octet_length(wrapped) BETWEEN 1 AND 512),
    PRIMARY KEY (vault_id, recipient_kind, recipient_id)
);

CREATE TABLE paperboat.recipe_vault_wraps (
    recipe_id text NOT NULL REFERENCES paperboat.recipes(id) ON DELETE CASCADE,
    vault_id text NOT NULL REFERENCES paperboat.environment_named_vaults(id) ON DELETE CASCADE,
    key_epoch integer NOT NULL,
    wrapped bytea NOT NULL CHECK (octet_length(wrapped) BETWEEN 1 AND 512),
    PRIMARY KEY (recipe_id, vault_id)
);
CREATE INDEX recipe_vault_wraps_vault ON paperboat.recipe_vault_wraps (vault_id);

-- After a rotate: the recipe's new private key for one machine, sealed to that machine's ENV
-- host-recipient key. The machine fetches it with its session and keeps reading vaults.
CREATE TABLE paperboat.recipe_machine_keys (
    machine_id text PRIMARY KEY REFERENCES paperboat.recipe_machines(machine_id) ON DELETE CASCADE,
    recipe_id text NOT NULL REFERENCES paperboat.recipes(id) ON DELETE CASCADE,
    sealed bytea NOT NULL CHECK (octet_length(sealed) BETWEEN 1 AND 512),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE paperboat.recipe_machine_keys;
DROP TABLE paperboat.recipe_vault_wraps;
DROP TABLE paperboat.environment_named_vault_wraps;
DROP TABLE paperboat.environment_named_vaults;
