-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy AFTER INSERT OR UPDATE OF suspended, deleted_at, owner_account, allow_private_access, generation OR DELETE ON paperboat.teams
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_team_row();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy ON paperboat.teams;
