-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy AFTER INSERT OR DELETE ON paperboat.user_machine_access_sessions
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.user_machine_access_sessions FOR EACH ROW
 WHEN ((OLD.user_machine_id, OLD.user_id, OLD.cli_client_session_id, OLD.state, OLD.revoked_at, OLD.expires_at, OLD.capabilities, OLD.team_id, OLD.terminal_session_id,
        OLD.terminal_role, OLD.terminal_grant_generation, OLD.terminal_binding_generation, OLD.terminal_membership_generation, OLD.machine_service_port)
   IS DISTINCT FROM (NEW.user_machine_id, NEW.user_id, NEW.cli_client_session_id, NEW.state, NEW.revoked_at, NEW.expires_at, NEW.capabilities, NEW.team_id, NEW.terminal_session_id,
        NEW.terminal_role, NEW.terminal_grant_generation, NEW.terminal_binding_generation, NEW.terminal_membership_generation, NEW.machine_service_port))
 EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy_update ON paperboat.user_machine_access_sessions;
DROP TRIGGER peer_policy ON paperboat.user_machine_access_sessions;
