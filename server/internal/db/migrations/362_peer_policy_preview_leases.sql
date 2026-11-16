-- +goose Up
-- Inspector access follows its preview lease: a change notifies the peer policy cache (361).
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
CREATE TRIGGER peer_policy_inspected AFTER DELETE ON paperboat.preview_leases
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_inspected();
CREATE TRIGGER peer_policy_inspected_update AFTER UPDATE ON paperboat.preview_leases FOR EACH ROW
 WHEN ((OLD.terminal_state, OLD.user_deadline, OLD.account_id) IS DISTINCT FROM (NEW.terminal_state, NEW.user_deadline, NEW.account_id)
   OR NEW.lease_deadline < OLD.lease_deadline)
 EXECUTE FUNCTION paperboat.publish_peer_policy_inspected();

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
DROP TRIGGER peer_policy_inspected_update ON paperboat.preview_leases;
DROP TRIGGER peer_policy_inspected ON paperboat.preview_leases;
