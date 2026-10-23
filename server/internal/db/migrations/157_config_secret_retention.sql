-- +goose Up
-- Encrypted config-sync credentials and repository-access tokens were kept forever after their
-- short expiry (audit D2: 10,678 and 8,346 expired rows, 0 live). The ciphertext is only needed for
-- an idempotent replay before expiry, so it is cleared at expiry and the row is deleted an hour
-- later (the controlplane config revocation worker keeps it that way). This migration clears the
-- backlog once. Prior art: Headscale hard-deletes expired OAuth tokens (hscontrol/db/oauth.go:282).
ALTER TABLE paperboat.control_config_credentials ALTER COLUMN credential_ciphertext DROP NOT NULL;
-- jti is what every lookup uses; jti_hash (the old primary key) was only ever written.
ALTER TABLE paperboat.control_config_credentials DROP CONSTRAINT control_config_credentials_pkey;
ALTER TABLE paperboat.control_config_credentials DROP CONSTRAINT control_config_credentials_jti_key;
ALTER TABLE paperboat.control_config_credentials ADD CONSTRAINT control_config_credentials_pkey PRIMARY KEY (jti);
ALTER TABLE paperboat.control_config_credentials DROP COLUMN jti_hash;
-- An issued access operation keeps its expiry; its token ciphertext is cleared once expired.
ALTER TABLE paperboat.control_config_repository_access_operations DROP CONSTRAINT control_config_repository_access_operations_check;
ALTER TABLE paperboat.control_config_repository_access_operations ADD CONSTRAINT control_config_repository_access_operations_check
    CHECK (state <> 'issued' OR expires_at IS NOT NULL);
CREATE INDEX control_config_credentials_expiry ON paperboat.control_config_credentials (expires_at);
CREATE INDEX control_config_repository_access_operations_expiry ON paperboat.control_config_repository_access_operations (expires_at);

DELETE FROM paperboat.control_config_credentials WHERE expires_at <= now() - interval '1 hour';
UPDATE paperboat.control_config_credentials SET credential_ciphertext = NULL
 WHERE expires_at <= now() AND credential_ciphertext IS NOT NULL;
DELETE FROM paperboat.control_config_repository_access_operations
 WHERE (expires_at IS NOT NULL AND expires_at <= now() - interval '1 hour')
    OR (expires_at IS NULL AND created_at <= now() - interval '1 day');
UPDATE paperboat.control_config_repository_access_operations SET access_ciphertext = NULL
 WHERE expires_at <= now() AND access_ciphertext IS NOT NULL;

-- +goose Down
DROP INDEX paperboat.control_config_repository_access_operations_expiry;
DROP INDEX paperboat.control_config_credentials_expiry;
ALTER TABLE paperboat.control_config_repository_access_operations DROP CONSTRAINT control_config_repository_access_operations_check;
ALTER TABLE paperboat.control_config_repository_access_operations ADD CONSTRAINT control_config_repository_access_operations_check
    CHECK ((state = 'issued' AND access_ciphertext IS NOT NULL AND expires_at IS NOT NULL) OR state <> 'issued') NOT VALID;
ALTER TABLE paperboat.control_config_credentials ADD COLUMN jti_hash bytea;
UPDATE paperboat.control_config_credentials SET jti_hash = sha256(convert_to(jti, 'UTF8'));
ALTER TABLE paperboat.control_config_credentials ALTER COLUMN jti_hash SET NOT NULL;
ALTER TABLE paperboat.control_config_credentials DROP CONSTRAINT control_config_credentials_pkey;
ALTER TABLE paperboat.control_config_credentials ADD CONSTRAINT control_config_credentials_jti_key UNIQUE (jti);
ALTER TABLE paperboat.control_config_credentials ADD CONSTRAINT control_config_credentials_pkey PRIMARY KEY (jti_hash);
