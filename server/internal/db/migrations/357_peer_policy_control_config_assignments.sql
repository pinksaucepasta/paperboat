-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy AFTER INSERT OR DELETE ON paperboat.control_config_assignments
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.control_config_assignments FOR EACH ROW
 WHEN ((OLD.version, OLD.machine_id, OLD.consent_state, OLD.adopted_team_id, OLD.revoked_at) IS DISTINCT FROM (NEW.version, NEW.machine_id, NEW.consent_state, NEW.adopted_team_id, NEW.revoked_at))
 EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy_update ON paperboat.control_config_assignments;
DROP TRIGGER peer_policy ON paperboat.control_config_assignments;
