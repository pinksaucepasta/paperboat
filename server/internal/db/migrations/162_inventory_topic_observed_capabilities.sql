-- +goose Up
-- After `pb update` the host runtime restarts, and the daemon's first inventory refresh lands before the
-- runtime re-reports its capabilities: file_receive/preview_launch read observed=false. The runtime reports
-- them seconds later, but no topic moved, so Transfer and Preview showed "degraded" for the 5-minute event
-- fallback, on the machine and on every peer (Lane P, Lane U). A change in the set of observed capabilities
-- now moves the inventory topic too. It changes at start-up and update only, never per heartbeat; a reordered
-- write of the same set does not count.
-- Prior art: Tailscale sends a peer change when its Hostinfo (services, capabilities) changes, not per ping.
DROP TRIGGER user_machines_publish_inventory_update ON paperboat.user_machines;
CREATE TRIGGER user_machines_publish_inventory_update
AFTER UPDATE OF alias, state, seat_state, deleted_at, revoked_at, owner_team_id, last_seen_at, observed_capabilities ON paperboat.user_machines
FOR EACH ROW WHEN (
  old.alias IS DISTINCT FROM new.alias
  OR (old.state IS DISTINCT FROM new.state AND NOT (old.state IN ('online','offline') AND new.state IN ('online','offline')))
  OR (old.last_seen_at IS NULL AND new.last_seen_at IS NOT NULL)
  OR old.seat_state IS DISTINCT FROM new.seat_state
  OR old.deleted_at IS DISTINCT FROM new.deleted_at
  OR old.revoked_at IS DISTINCT FROM new.revoked_at
  OR old.owner_team_id IS DISTINCT FROM new.owner_team_id
  OR NOT (coalesce(old.observed_capabilities, '{}') @> coalesce(new.observed_capabilities, '{}')
      AND coalesce(new.observed_capabilities, '{}') @> coalesce(old.observed_capabilities, '{}')))
EXECUTE FUNCTION paperboat.publish_machine_inventory();

-- +goose Down
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
