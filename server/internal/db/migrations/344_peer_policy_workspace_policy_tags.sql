-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy_insert AFTER INSERT ON paperboat.workspace_policy_tags REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_peer_policy_workspace();
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.workspace_policy_tags REFERENCING NEW TABLE AS new_rows OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_peer_policy_workspace();
CREATE TRIGGER peer_policy_delete AFTER DELETE ON paperboat.workspace_policy_tags REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_peer_policy_workspace();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy_insert ON paperboat.workspace_policy_tags;
DROP TRIGGER peer_policy_update ON paperboat.workspace_policy_tags;
DROP TRIGGER peer_policy_delete ON paperboat.workspace_policy_tags;
