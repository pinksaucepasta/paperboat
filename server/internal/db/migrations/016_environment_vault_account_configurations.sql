-- +goose Up
CREATE TABLE paperboat.environment_vault_account_configurations (
  account_id text PRIMARY KEY REFERENCES paperboat.users(id) ON DELETE CASCADE,
  generation bigint NOT NULL DEFAULT 1,
  configured_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT environment_vault_account_configurations_generation_check
    CHECK (generation >= 1 AND generation <= 9007199254740991)
);

-- Existing vault and host records prove that the account configured ENV before
-- this marker existed. Include all durable personal-vault traces so deleted
-- vault heads cannot be mistaken for a never-configured account.
INSERT INTO paperboat.environment_vault_account_configurations(account_id, generation)
SELECT DISTINCT legacy.account_id, 1
FROM (
  SELECT account_id FROM paperboat.environment_password_vaults
  UNION SELECT account_id FROM paperboat.environment_vault_hosts
  UNION SELECT account_id FROM paperboat.environment_vault_operations
  UNION SELECT account_id FROM paperboat.environment_vault_personal_epochs
  UNION SELECT account_id FROM paperboat.environment_vault_personal_rotations
  UNION SELECT account_id FROM paperboat.environment_vault_personal_rotation_scopes
  UNION SELECT owner_id AS account_id FROM paperboat.environment_vault_scopes WHERE owner_kind = 'personal'
  UNION SELECT account_id FROM paperboat.environment_vault_team_members
  UNION SELECT account_id FROM paperboat.environment_vault_team_grants
) AS legacy
JOIN paperboat.users AS u ON u.id = legacy.account_id
ON CONFLICT (account_id) DO NOTHING;

-- +goose StatementBegin
CREATE FUNCTION paperboat.mark_environment_vault_account_configured() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO paperboat.environment_vault_account_configurations(account_id, generation)
  VALUES (NEW.account_id, 1)
  ON CONFLICT (account_id) DO NOTHING;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER environment_password_vaults_mark_account_configured
AFTER INSERT OR UPDATE ON paperboat.environment_password_vaults
FOR EACH ROW EXECUTE FUNCTION paperboat.mark_environment_vault_account_configured();
