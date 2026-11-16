-- +goose Up
-- Inspector access follows its tunnel: a change notifies the peer policy cache (361).
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
CREATE TRIGGER peer_policy_inspected AFTER DELETE ON paperboat.tunnels
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_inspected();
CREATE TRIGGER peer_policy_inspected_update AFTER UPDATE ON paperboat.tunnels FOR EACH ROW
 WHEN ((OLD.generation, OLD.desired_state, OLD.deleted_at, OLD.expires_at, OLD.account_id, OLD.created_by_host_id)
   IS DISTINCT FROM (NEW.generation, NEW.desired_state, NEW.deleted_at, NEW.expires_at, NEW.account_id, NEW.created_by_host_id))
 EXECUTE FUNCTION paperboat.publish_peer_policy_inspected();

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
DROP TRIGGER peer_policy_inspected_update ON paperboat.tunnels;
DROP TRIGGER peer_policy_inspected ON paperboat.tunnels;
