-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy AFTER INSERT OR UPDATE OR DELETE ON paperboat.recording_native_admissions
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy ON paperboat.recording_native_admissions;
