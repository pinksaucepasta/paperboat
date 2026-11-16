-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy AFTER DELETE ON paperboat.cli_client_sessions
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.cli_client_sessions FOR EACH ROW
 WHEN ((OLD.state, OLD.revoked_at, OLD.recipe_id, OLD.user_id) IS DISTINCT FROM (NEW.state, NEW.revoked_at, NEW.recipe_id, NEW.user_id))
 EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy_update ON paperboat.cli_client_sessions;
DROP TRIGGER peer_policy ON paperboat.cli_client_sessions;
