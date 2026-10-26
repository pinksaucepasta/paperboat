-- +goose Up
-- A revoked or deleted machine leaves every team it was shared into, on every revoke path (user,
-- entitlement, seat limit, delete): its bindings end and its pending share requests go, in the same
-- transaction. Before this a revoked machine kept its share, and unsharing it answered not_found.
-- +goose StatementBegin
CREATE FUNCTION paperboat.end_team_entries_of_ended_machine() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE paperboat.team_resource_bindings SET active=false, generation=generation+1
 WHERE resource_kind='machine' AND resource_id=NEW.id AND active;
 DELETE FROM paperboat.team_machine_requests WHERE machine_id=NEW.id;
 RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER team_entries_of_ended_machine AFTER UPDATE OF state, revoked_at, deleted_at ON paperboat.user_machines
 FOR EACH ROW WHEN ((old.revoked_at IS NULL AND new.revoked_at IS NOT NULL) OR (old.deleted_at IS NULL AND new.deleted_at IS NOT NULL)
  OR (old.state NOT IN ('revoked','deleted') AND new.state IN ('revoked','deleted')))
 EXECUTE FUNCTION paperboat.end_team_entries_of_ended_machine();

-- +goose Down
DROP TRIGGER team_entries_of_ended_machine ON paperboat.user_machines;
DROP FUNCTION paperboat.end_team_entries_of_ended_machine();
