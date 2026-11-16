-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy AFTER INSERT OR DELETE ON paperboat.user_machine_terminal_sessions
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.user_machine_terminal_sessions FOR EACH ROW
 WHEN ((OLD.user_machine_id, OLD.owner_account, OLD.owner_team_id, OLD.deleted_at, OLD.desired_state, OLD.closed_at, OLD.transfer_destination_machine_id)
   IS DISTINCT FROM (NEW.user_machine_id, NEW.owner_account, NEW.owner_team_id, NEW.deleted_at, NEW.desired_state, NEW.closed_at, NEW.transfer_destination_machine_id))
 EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy_update ON paperboat.user_machine_terminal_sessions;
DROP TRIGGER peer_policy ON paperboat.user_machine_terminal_sessions;
