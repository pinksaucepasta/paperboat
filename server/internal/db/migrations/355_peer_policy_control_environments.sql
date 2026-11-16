-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy AFTER INSERT OR DELETE ON paperboat.control_environments
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.control_environments FOR EACH ROW
 WHEN ((OLD.desired_state, OLD.revoked_at, OLD.owner_user_id) IS DISTINCT FROM (NEW.desired_state, NEW.revoked_at, NEW.owner_user_id))
 EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy_update ON paperboat.control_environments;
DROP TRIGGER peer_policy ON paperboat.control_environments;
