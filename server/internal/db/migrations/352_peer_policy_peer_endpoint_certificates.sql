-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy AFTER INSERT OR UPDATE OR DELETE ON paperboat.peer_endpoint_certificates
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy ON paperboat.peer_endpoint_certificates;
