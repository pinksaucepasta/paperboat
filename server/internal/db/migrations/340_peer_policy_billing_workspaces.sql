-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy AFTER INSERT OR DELETE ON paperboat.billing_workspaces
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.billing_workspaces FOR EACH ROW
 WHEN ((OLD.team_id, OLD.user_id, OLD.state, OLD.period_start, OLD.period_end, OLD.renewal_grace_until)
   IS DISTINCT FROM (NEW.team_id, NEW.user_id, NEW.state, NEW.period_start, NEW.period_end, NEW.renewal_grace_until))
 EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy_update ON paperboat.billing_workspaces;
DROP TRIGGER peer_policy ON paperboat.billing_workspaces;
