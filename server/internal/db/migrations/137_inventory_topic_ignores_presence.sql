-- +goose Up
-- The inventory topic is names, aliases and membership. A presence flip (online <-> offline) bumped it for every
-- machine in the audience, so every daemon of an account re-listed the account on each flip (~4 lists/s fleet-wide,
-- the largest database load). Presence reaches the one command that waits on it (pb wait) by asking its own daemon.
DROP TRIGGER user_machines_publish_inventory_update ON paperboat.user_machines;
CREATE TRIGGER user_machines_publish_inventory_update
AFTER UPDATE OF alias, state, seat_state, deleted_at, revoked_at, owner_team_id ON paperboat.user_machines
FOR EACH ROW WHEN (
  old.alias IS DISTINCT FROM new.alias
  OR (old.state IS DISTINCT FROM new.state AND NOT (old.state IN ('online','offline') AND new.state IN ('online','offline')))
  OR old.seat_state IS DISTINCT FROM new.seat_state
  OR old.deleted_at IS DISTINCT FROM new.deleted_at
  OR old.revoked_at IS DISTINCT FROM new.revoked_at
  OR old.owner_team_id IS DISTINCT FROM new.owner_team_id)
EXECUTE FUNCTION paperboat.publish_machine_inventory();

-- +goose Down
DROP TRIGGER user_machines_publish_inventory_update ON paperboat.user_machines;
CREATE TRIGGER user_machines_publish_inventory_update
AFTER UPDATE OF alias, state, online, seat_state, deleted_at, revoked_at, owner_team_id ON paperboat.user_machines
FOR EACH ROW WHEN (
  old.alias IS DISTINCT FROM new.alias OR old.state IS DISTINCT FROM new.state OR old.online IS DISTINCT FROM new.online
  OR old.seat_state IS DISTINCT FROM new.seat_state OR old.deleted_at IS DISTINCT FROM new.deleted_at
  OR old.revoked_at IS DISTINCT FROM new.revoked_at OR old.owner_team_id IS DISTINCT FROM new.owner_team_id)
EXECUTE FUNCTION paperboat.publish_machine_inventory();
