-- +goose Up
-- A host refuses managed SSH certificates whose holder lost access through its revocation token, recomputed when its
-- machine_access topic moves. Revoking a CLI session and removing or deactivating a team member now also move it on
-- every machine the session or member holds a still-valid certificate for (managed_ssh_certificate_holders), so the
-- refusal reaches the host within one push. Grant withdrawals already publish machine_access on the machine (206).
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_cli_session_revoke_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE hosts text[];
BEGIN
  hosts := ARRAY(
    SELECT m.id FROM paperboat.user_machines m
     WHERE m.user_id = NEW.user_id AND m.owner_team_id IS NULL AND m.revoked_at IS NULL AND m.deleted_at IS NULL
    UNION
    SELECT s.user_machine_id FROM paperboat.user_machine_access_sessions s WHERE s.cli_client_session_id = NEW.id
    UNION
    SELECT h.machine_id FROM paperboat.managed_ssh_certificate_holders h WHERE h.cli_client_session_id = NEW.id);
  PERFORM paperboat.publish_machine_topic(hosts, 'peer_network');
  PERFORM paperboat.publish_machine_topic(hosts, 'machine_access');
  RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_memberships() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE changed text[];
BEGIN
  IF TG_OP = 'INSERT' THEN
    changed := ARRAY(SELECT DISTINCT account_id FROM new_members);
  ELSIF TG_OP = 'DELETE' THEN
    changed := ARRAY(SELECT DISTINCT account_id FROM old_members);
  ELSE
    changed := ARRAY(SELECT DISTINCT n.account_id FROM new_members n
      JOIN old_members o ON o.team_id = n.team_id AND o.account_id = n.account_id
      WHERE o.active IS DISTINCT FROM n.active OR o.role IS DISTINCT FROM n.role);
  END IF;
  PERFORM paperboat.publish_account_topic(changed, 'inventory');
  PERFORM paperboat.publish_account_topic(changed, 'machine_access');
  IF TG_OP <> 'INSERT' THEN
    PERFORM paperboat.publish_machine_topic(ARRAY(SELECT DISTINCT h.machine_id FROM paperboat.managed_ssh_certificate_holders h
      WHERE h.user_id = ANY(changed)), 'machine_access');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_cli_session_revoke_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE hosts text[];
BEGIN
  hosts := ARRAY(
    SELECT m.id FROM paperboat.user_machines m
     WHERE m.user_id = NEW.user_id AND m.owner_team_id IS NULL AND m.revoked_at IS NULL AND m.deleted_at IS NULL
    UNION
    SELECT s.user_machine_id FROM paperboat.user_machine_access_sessions s WHERE s.cli_client_session_id = NEW.id);
  PERFORM paperboat.publish_machine_topic(hosts, 'peer_network');
  PERFORM paperboat.publish_machine_topic(hosts, 'machine_access');
  RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_memberships() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE changed text[];
BEGIN
  IF TG_OP = 'INSERT' THEN
    changed := ARRAY(SELECT DISTINCT account_id FROM new_members);
  ELSIF TG_OP = 'DELETE' THEN
    changed := ARRAY(SELECT DISTINCT account_id FROM old_members);
  ELSE
    changed := ARRAY(SELECT DISTINCT n.account_id FROM new_members n
      JOIN old_members o ON o.team_id = n.team_id AND o.account_id = n.account_id
      WHERE o.active IS DISTINCT FROM n.active OR o.role IS DISTINCT FROM n.role);
  END IF;
  PERFORM paperboat.publish_account_topic(changed, 'inventory');
  PERFORM paperboat.publish_account_topic(changed, 'machine_access');
  RETURN NULL;
END $$;
-- +goose StatementEnd
