-- +goose Up
-- 162 moved the inventory topic on every change of a machine's observed capabilities set. Two writers
-- report different sets for the same machine (runtime observation vs heartbeat; e.g. {..., keep_awake,
-- environment_injection} vs {..., ssh_host}), so the set flapped on every write: ~5 inventory moves a
-- second, every peer's event wait woke at once and refetched its machine list, and prod PostgreSQL ran
-- at ~300% CPU from 23:12Z with edge control requests timing out. Back to 145's trigger until the
-- capability set has one writer.
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
