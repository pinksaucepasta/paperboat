-- +goose Up
-- Revoking a CLI session (pb logout) must end its open connections within seconds: a host drops the peer at its
-- next peer_network refresh, and nothing woke that refresh. The personal network lists every CLI session as a peer on
-- all of its account's own machines, and a team access session names the machines of another account. Only the revoke
-- publishes: config_generation updates (which the identity triggers make on their own) must not, or the two would loop.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_cli_session_revoke_peer_network() RETURNS trigger
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
CREATE TRIGGER cli_client_sessions_publish_revoke_peer_network
    AFTER UPDATE OF state, revoked_at ON paperboat.cli_client_sessions
    FOR EACH ROW WHEN (NEW.state = 'revoked' AND OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION paperboat.publish_cli_session_revoke_peer_network();

-- +goose Down
DROP TRIGGER cli_client_sessions_publish_revoke_peer_network ON paperboat.cli_client_sessions;
DROP FUNCTION paperboat.publish_cli_session_revoke_peer_network();
