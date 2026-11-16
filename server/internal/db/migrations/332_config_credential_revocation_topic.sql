-- +goose Up
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_config_credential_revocations() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY(
    SELECT DISTINCT n.machine_id FROM new_rows n JOIN old_rows o ON o.jti = n.jti
    WHERE n.revoked_at IS NOT NULL AND o.revoked_at IS DISTINCT FROM n.revoked_at
  ), 'machine_access');
  RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER control_config_credentials_revoked AFTER UPDATE ON paperboat.control_config_credentials
  REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
  FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_config_credential_revocations();

-- +goose Down
DROP TRIGGER control_config_credentials_revoked ON paperboat.control_config_credentials;
DROP FUNCTION paperboat.publish_config_credential_revocations();
