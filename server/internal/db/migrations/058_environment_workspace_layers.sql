-- +goose Up
ALTER TABLE paperboat.environment_vault_scopes ADD COLUMN workspace_id text NOT NULL DEFAULT 'personal';
UPDATE paperboat.environment_vault_scopes SET workspace_id=owner_id WHERE owner_kind='team';
ALTER TABLE paperboat.environment_vault_scopes ALTER COLUMN workspace_id DROP DEFAULT;
ALTER TABLE paperboat.environment_vault_scopes DROP CONSTRAINT environment_vault_scopes_pkey;
ALTER TABLE paperboat.environment_vault_scopes ADD PRIMARY KEY(workspace_id,owner_kind,owner_id,machine_id);
ALTER TABLE paperboat.environment_vault_personal_rotation_scopes ADD COLUMN workspace_id text NOT NULL DEFAULT 'personal';
ALTER TABLE paperboat.environment_vault_personal_rotation_scopes ALTER COLUMN workspace_id DROP DEFAULT;
ALTER TABLE paperboat.environment_vault_personal_rotation_scopes DROP CONSTRAINT environment_vault_personal_rotation_scopes_pkey;
ALTER TABLE paperboat.environment_vault_personal_rotation_scopes ADD PRIMARY KEY(account_id,workspace_id,machine_id);
CREATE TABLE paperboat.environment_vault_layer_fences (
 workspace_id text NOT NULL, owner_kind text NOT NULL, owner_id text NOT NULL, source_machine_id text NOT NULL,
 generation bigint NOT NULL CHECK(generation BETWEEN 1 AND 9007199254740991),
 PRIMARY KEY(workspace_id,owner_kind,owner_id,source_machine_id),
 FOREIGN KEY(workspace_id,owner_kind,owner_id,source_machine_id) REFERENCES paperboat.environment_vault_scopes(workspace_id,owner_kind,owner_id,machine_id) ON DELETE CASCADE
);
CREATE TABLE paperboat.environment_vault_layers (
 machine_id text NOT NULL REFERENCES paperboat.environment_vault_hosts(machine_id) ON DELETE CASCADE,
 workspace_id text NOT NULL, owner_kind text NOT NULL, owner_id text NOT NULL, source_machine_id text NOT NULL,
 delivery_generation bigint NOT NULL CHECK(delivery_generation BETWEEN 1 AND 9007199254740991),
 document_id text NOT NULL CHECK(document_id ~ '^sha256:[a-f0-9]{64}$'),
 installation_generation bigint NOT NULL CHECK(installation_generation>0), host_key_generation bigint NOT NULL CHECK(host_key_generation>0),
 applied_fence_generation bigint NOT NULL CHECK(applied_fence_generation>0), writer_account text NOT NULL REFERENCES paperboat.users(id),
 writer_public bytea NOT NULL CHECK(octet_length(writer_public)=32), envelope bytea NOT NULL CHECK(octet_length(envelope) BETWEEN 1 AND 327680),
 PRIMARY KEY(machine_id,workspace_id,owner_kind,owner_id,source_machine_id),
 FOREIGN KEY(workspace_id,owner_kind,owner_id,source_machine_id) REFERENCES paperboat.environment_vault_scopes(workspace_id,owner_kind,owner_id,machine_id) ON DELETE CASCADE
);
INSERT INTO paperboat.environment_vault_layer_fences SELECT workspace_id,owner_kind,owner_id,machine_id,1 FROM paperboat.environment_vault_scopes;
-- +goose Down
-- Scope envelopes bind workspace_id in authenticated v1 claims. Reverting the
-- schema cannot rewrite those encrypted claims, even for Personal scopes.
-- Refuse before changing any table when authoritative scoped material exists.
-- +goose StatementBegin
DO $$
BEGIN
 IF EXISTS(SELECT 1 FROM paperboat.environment_vault_scopes)
 OR EXISTS(SELECT 1 FROM paperboat.environment_vault_personal_rotation_scopes) THEN
  RAISE EXCEPTION 'workspace-scoped encrypted ENV material prevents downgrade; use the supported release rollback policy';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE paperboat.environment_vault_layers;
DROP TABLE paperboat.environment_vault_layer_fences;
ALTER TABLE paperboat.environment_vault_scopes DROP CONSTRAINT environment_vault_scopes_pkey;
ALTER TABLE paperboat.environment_vault_scopes DROP COLUMN workspace_id;
ALTER TABLE paperboat.environment_vault_scopes ADD PRIMARY KEY(owner_kind,owner_id,machine_id);
ALTER TABLE paperboat.environment_vault_personal_rotation_scopes DROP CONSTRAINT environment_vault_personal_rotation_scopes_pkey;
ALTER TABLE paperboat.environment_vault_personal_rotation_scopes DROP COLUMN workspace_id;
ALTER TABLE paperboat.environment_vault_personal_rotation_scopes ADD PRIMARY KEY(account_id,machine_id);
