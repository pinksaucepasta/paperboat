-- +goose Up
-- PR-03: an idle machine asks for its availability policy and its managed SSH keys only when they change,
-- so the writes that change them publish the topics the host waits on.

-- config: the machine's availability policy (POST /v1/helper-runtime-policies/resolve).
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_machine_availability() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY[NEW.id], 'config');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER user_machines_publish_availability
    AFTER UPDATE OF availability_mode, availability_desired_version ON paperboat.user_machines
    FOR EACH ROW WHEN (OLD.availability_mode IS DISTINCT FROM NEW.availability_mode
        OR OLD.availability_desired_version IS DISTINCT FROM NEW.availability_desired_version)
    EXECUTE FUNCTION paperboat.publish_machine_availability();

-- machine_access: a managed SSH client key is accepted on every machine its account may reach, so a key
-- change wakes the account's own machines, its teams' machines and the machines bound into its teams.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_managed_ssh_account(account text) RETURNS void
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY[account], 'machine_access');
  PERFORM paperboat.publish_machine_topic(ARRAY(
    SELECT own.id FROM paperboat.user_machines own
     JOIN paperboat.team_members member ON member.team_id = own.owner_team_id AND member.active
     WHERE member.account_id = account AND own.deleted_at IS NULL AND own.revoked_at IS NULL
    UNION
    SELECT b.resource_id FROM paperboat.team_resource_bindings b
     JOIN paperboat.team_members member ON member.team_id = b.team_id AND member.active
     WHERE member.account_id = account AND b.resource_kind = 'machine' AND b.active), 'machine_access');
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_managed_ssh_client_key() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_managed_ssh_account(NEW.user_id);
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER managed_ssh_client_keys_publish_insert
    AFTER INSERT ON paperboat.managed_ssh_client_keys
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_managed_ssh_client_key();
CREATE TRIGGER managed_ssh_client_keys_publish_update
    AFTER UPDATE OF state, public_key ON paperboat.managed_ssh_client_keys
    FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state OR OLD.public_key IS DISTINCT FROM NEW.public_key)
    EXECUTE FUNCTION paperboat.publish_managed_ssh_client_key();

-- A key is served only while its CLI session was used within the active window (days). A session used
-- again after more than a day idle may have left that window, so its use publishes too; a session in
-- daily use never does. Leaving the window has no write; hosts re-check on a slow fallback.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_managed_ssh_session_reuse() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS(SELECT 1 FROM paperboat.managed_ssh_client_keys k
             WHERE k.cli_client_session_id = NEW.id AND k.state = 'active') THEN
    PERFORM paperboat.publish_managed_ssh_account(NEW.user_id);
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER cli_client_sessions_publish_managed_ssh_reuse
    AFTER UPDATE OF last_used_at ON paperboat.cli_client_sessions
    FOR EACH ROW WHEN (NEW.last_used_at > coalesce(OLD.last_used_at, OLD.created_at) + interval '1 day')
    EXECUTE FUNCTION paperboat.publish_managed_ssh_session_reuse();

-- +goose Down
DROP TRIGGER cli_client_sessions_publish_managed_ssh_reuse ON paperboat.cli_client_sessions;
DROP FUNCTION paperboat.publish_managed_ssh_session_reuse();
DROP TRIGGER managed_ssh_client_keys_publish_update ON paperboat.managed_ssh_client_keys;
DROP TRIGGER managed_ssh_client_keys_publish_insert ON paperboat.managed_ssh_client_keys;
DROP FUNCTION paperboat.publish_managed_ssh_client_key();
DROP FUNCTION paperboat.publish_managed_ssh_account(text);
DROP TRIGGER user_machines_publish_availability ON paperboat.user_machines;
DROP FUNCTION paperboat.publish_machine_availability();
