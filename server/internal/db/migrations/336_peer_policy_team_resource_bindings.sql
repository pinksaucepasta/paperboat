-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy_insert AFTER INSERT ON paperboat.team_resource_bindings REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_peer_policy_teams();
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.team_resource_bindings REFERENCING NEW TABLE AS new_rows OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_peer_policy_teams();
CREATE TRIGGER peer_policy_delete AFTER DELETE ON paperboat.team_resource_bindings REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_peer_policy_teams();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy_insert ON paperboat.team_resource_bindings;
DROP TRIGGER peer_policy_update ON paperboat.team_resource_bindings;
DROP TRIGGER peer_policy_delete ON paperboat.team_resource_bindings;
