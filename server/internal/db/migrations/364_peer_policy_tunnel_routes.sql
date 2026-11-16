-- +goose Up
-- Inspector access follows its tunnel route: a change notifies the peer policy cache (361).
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
CREATE TRIGGER peer_policy_inspected AFTER INSERT OR DELETE ON paperboat.tunnel_routes
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_inspected();
CREATE TRIGGER peer_policy_inspected_update AFTER UPDATE ON paperboat.tunnel_routes FOR EACH ROW
 WHEN ((OLD.generation, OLD.desired_state, OLD.deleted_at) IS DISTINCT FROM (NEW.generation, NEW.desired_state, NEW.deleted_at))
 EXECUTE FUNCTION paperboat.publish_peer_policy_inspected();

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
DROP TRIGGER peer_policy_inspected_update ON paperboat.tunnel_routes;
DROP TRIGGER peer_policy_inspected ON paperboat.tunnel_routes;
