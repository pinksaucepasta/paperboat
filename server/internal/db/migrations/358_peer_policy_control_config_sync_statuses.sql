-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy AFTER INSERT OR DELETE ON paperboat.control_config_sync_statuses
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.control_config_sync_statuses FOR EACH ROW
 WHEN ((OLD.installation_generation, OLD.remote_revision, OLD.conflicts) IS DISTINCT FROM (NEW.installation_generation, NEW.remote_revision, NEW.conflicts))
 EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy_update ON paperboat.control_config_sync_statuses;
DROP TRIGGER peer_policy ON paperboat.control_config_sync_statuses;
