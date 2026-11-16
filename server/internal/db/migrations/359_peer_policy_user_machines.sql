-- +goose Up
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
CREATE TRIGGER peer_policy AFTER INSERT OR DELETE ON paperboat.user_machines
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_machine();
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.user_machines FOR EACH ROW
 WHEN ((OLD.state, OLD.online, OLD.seat_state, OLD.revoked_at, OLD.deleted_at, OLD.installation_generation, OLD.environment_id, OLD.owner_team_id, OLD.user_id, OLD.configured_capabilities, OLD.observed_capabilities)
   IS DISTINCT FROM (NEW.state, NEW.online, NEW.seat_state, NEW.revoked_at, NEW.deleted_at, NEW.installation_generation, NEW.environment_id, NEW.owner_team_id, NEW.user_id, NEW.configured_capabilities, NEW.observed_capabilities))
 EXECUTE FUNCTION paperboat.publish_peer_policy_machine();

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
DROP TRIGGER peer_policy_update ON paperboat.user_machines;
DROP TRIGGER peer_policy ON paperboat.user_machines;
