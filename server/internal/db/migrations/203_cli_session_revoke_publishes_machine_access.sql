-- +goose Up
-- F5-10: a host serving SSH must learn within seconds that a CLI session was revoked. Besides peer_network (migration
-- 202) the revoke also moves machine_access of the same hosts: grants and access-session changes already publish it, and
-- a refresh never does, so a host can wake its signed-network renewal on machine_access alone without feeding itself
-- (waking on peer_network or inventory looped: refreshes report endpoints, and migration 186 publishes those).
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

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_cli_session_revoke_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY(
    SELECT m.id FROM paperboat.user_machines m
     WHERE m.user_id = NEW.user_id AND m.owner_team_id IS NULL AND m.revoked_at IS NULL AND m.deleted_at IS NULL
    UNION
    SELECT s.user_machine_id FROM paperboat.user_machine_access_sessions s WHERE s.cli_client_session_id = NEW.id), 'peer_network');
  RETURN NEW;
END $$;
-- +goose StatementEnd
