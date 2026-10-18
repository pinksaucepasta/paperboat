-- +goose Up
-- A removed machine or ended CLI session keeps no working peer-network identity: the row is revoked in
-- the same transaction that removes its owner (every removal path, not only the cleanup helper), and
-- the existing identity fence advances every affected network configuration.
-- +goose StatementBegin
CREATE FUNCTION paperboat.revoke_removed_machine_peer_identity() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  UPDATE paperboat.peer_network_identities SET revoked_at = now()
  WHERE role = 'machine' AND endpoint_id = NEW.id AND revoked_at IS NULL;
  RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.revoke_ended_cli_peer_identity() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  UPDATE paperboat.peer_network_identities SET revoked_at = now()
  WHERE role = 'cli' AND endpoint_id = NEW.id AND revoked_at IS NULL;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER machine_removed_peer_identity
  AFTER UPDATE OF state, revoked_at, deleted_at ON paperboat.user_machines
  FOR EACH ROW
  WHEN (NEW.state IN ('revoked', 'disconnected', 'deleted') OR NEW.revoked_at IS NOT NULL OR NEW.deleted_at IS NOT NULL)
  EXECUTE FUNCTION paperboat.revoke_removed_machine_peer_identity();
CREATE TRIGGER cli_session_ended_peer_identity
  AFTER UPDATE OF state, revoked_at ON paperboat.cli_client_sessions
  FOR EACH ROW
  WHEN (NEW.state <> 'active' OR NEW.revoked_at IS NOT NULL)
  EXECUTE FUNCTION paperboat.revoke_ended_cli_peer_identity();
UPDATE paperboat.peer_network_identities n SET revoked_at = now()
FROM paperboat.user_machines m
WHERE n.role = 'machine' AND n.revoked_at IS NULL AND m.id = n.endpoint_id
  AND (m.state IN ('revoked', 'disconnected', 'deleted') OR m.revoked_at IS NOT NULL OR m.deleted_at IS NOT NULL);
UPDATE paperboat.peer_network_identities n SET revoked_at = now()
FROM paperboat.cli_client_sessions c
WHERE n.role = 'cli' AND n.revoked_at IS NULL AND c.id = n.endpoint_id
  AND (c.state <> 'active' OR c.revoked_at IS NOT NULL);

-- +goose Down
DROP TRIGGER cli_session_ended_peer_identity ON paperboat.cli_client_sessions;
DROP TRIGGER machine_removed_peer_identity ON paperboat.user_machines;
DROP FUNCTION paperboat.revoke_ended_cli_peer_identity();
DROP FUNCTION paperboat.revoke_removed_machine_peer_identity();
