-- +goose Up
-- A newly paired machine is created offline and never seen (CreateEnrolledMachine); its first heartbeat is an
-- offline -> online flip, which 137 stopped publishing, so its peers kept it cached as offline for up to the event
-- fallback (pb exec refused it for ~2 min after pairing). The first time a machine is seen moves the topic again;
-- later presence flips still do not.
DROP TRIGGER user_machines_publish_inventory_update ON paperboat.user_machines;
CREATE TRIGGER user_machines_publish_inventory_update
AFTER UPDATE OF alias, state, seat_state, deleted_at, revoked_at, owner_team_id, last_seen_at ON paperboat.user_machines
FOR EACH ROW WHEN (
  old.alias IS DISTINCT FROM new.alias
  OR (old.state IS DISTINCT FROM new.state AND NOT (old.state IN ('online','offline') AND new.state IN ('online','offline')))
  OR (old.last_seen_at IS NULL AND new.last_seen_at IS NOT NULL)
  OR old.seat_state IS DISTINCT FROM new.seat_state
  OR old.deleted_at IS DISTINCT FROM new.deleted_at
  OR old.revoked_at IS DISTINCT FROM new.revoked_at
  OR old.owner_team_id IS DISTINCT FROM new.owner_team_id)
EXECUTE FUNCTION paperboat.publish_machine_inventory();

-- +goose Down
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
