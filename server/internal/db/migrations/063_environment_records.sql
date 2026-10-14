-- +goose Up
ALTER TABLE paperboat.environment_vault_personal_rotation_scopes ADD COLUMN record_replacement jsonb NOT NULL DEFAULT '{"expected_sequence":0,"document_ids":[],"envelopes":[]}';
CREATE TABLE paperboat.environment_vault_record_heads (
 workspace_id text NOT NULL, owner_kind text NOT NULL, owner_id text NOT NULL, machine_id text NOT NULL,
 key_epoch bigint NOT NULL CHECK(key_epoch BETWEEN 1 AND 9007199254740991),
 sequence bigint NOT NULL DEFAULT 0 CHECK(sequence BETWEEN 0 AND 9007199254740991),
 floor_sequence bigint NOT NULL DEFAULT 0 CHECK(floor_sequence BETWEEN 0 AND sequence),
 cursor_key bytea NOT NULL CHECK(octet_length(cursor_key)=32),
 PRIMARY KEY(workspace_id,owner_kind,owner_id,machine_id),
 FOREIGN KEY(workspace_id,owner_kind,owner_id,machine_id) REFERENCES paperboat.environment_vault_scopes(workspace_id,owner_kind,owner_id,machine_id) ON DELETE CASCADE
);
CREATE TABLE paperboat.environment_vault_record_versions (
 workspace_id text NOT NULL, owner_kind text NOT NULL, owner_id text NOT NULL, machine_id text NOT NULL,
 key_epoch bigint NOT NULL CHECK(key_epoch BETWEEN 1 AND 9007199254740991),
 record_id text NOT NULL CHECK(record_id ~ '^[a-f0-9]{64}$'),
 revision bigint NOT NULL CHECK(revision BETWEEN 1 AND 9007199254740991),
 deleted boolean NOT NULL, sequence bigint NOT NULL CHECK(sequence BETWEEN 1 AND 9007199254740991),
 document_id text NOT NULL CHECK(document_id ~ '^sha256:[a-f0-9]{64}$'),
 envelope bytea NOT NULL CHECK(octet_length(envelope) BETWEEN 1 AND 34943),
 ciphertext_bytes integer NOT NULL CHECK(ciphertext_bytes BETWEEN 17 AND 32927),
 writer_public bytea NOT NULL CHECK(octet_length(writer_public)=32),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(workspace_id,owner_kind,owner_id,machine_id,key_epoch,record_id,revision),
 FOREIGN KEY(workspace_id,owner_kind,owner_id,machine_id) REFERENCES paperboat.environment_vault_record_heads(workspace_id,owner_kind,owner_id,machine_id) ON DELETE CASCADE
);
CREATE INDEX environment_vault_record_delta ON paperboat.environment_vault_record_versions(workspace_id,owner_kind,owner_id,machine_id,key_epoch,sequence,record_id);
-- +goose Down
DROP TABLE paperboat.environment_vault_record_versions;
DROP TABLE paperboat.environment_vault_record_heads;
ALTER TABLE paperboat.environment_vault_personal_rotation_scopes DROP COLUMN record_replacement;
