-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.users FOR EACH ROW
 WHEN (OLD.status IS DISTINCT FROM NEW.status)
 EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy_update ON paperboat.users;
