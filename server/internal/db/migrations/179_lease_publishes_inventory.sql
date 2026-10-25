-- +goose Up
-- A machine's local-address lease reaches its daemon through the machine list (inventory topic). A lease the
-- server adds after the daemon started (backfill, a pairing path that allocates late) moved no topic, so the
-- daemon kept failing to bind the address until it was restarted (prod 2026-10-25, Lane H). Gaining, moving or
-- losing a machine's lease now moves the inventory topic of the machine's audience. It happens at pairing and
-- release only, never per list or heartbeat.
-- Prior art: Tailscale sends a node's new addresses to its peers in the next netmap delta when control assigns them.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_lease_inventory() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.machine_id IS NOT NULL THEN
    PERFORM paperboat.publish_account_topic(paperboat.machine_audience_accounts(NEW.machine_id), 'inventory');
  END IF;
  IF TG_OP = 'UPDATE' AND OLD.machine_id IS NOT NULL AND OLD.machine_id IS DISTINCT FROM NEW.machine_id THEN
    PERFORM paperboat.publish_account_topic(paperboat.machine_audience_accounts(OLD.machine_id), 'inventory');
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER control_sync_leases_publish_inventory_insert AFTER INSERT ON paperboat.control_sync_leases
FOR EACH ROW WHEN (NEW.machine_id IS NOT NULL) EXECUTE FUNCTION paperboat.publish_lease_inventory();
CREATE TRIGGER control_sync_leases_publish_inventory_update AFTER UPDATE OF machine_id, address_slot ON paperboat.control_sync_leases
FOR EACH ROW WHEN (OLD.machine_id IS DISTINCT FROM NEW.machine_id OR OLD.address_slot IS DISTINCT FROM NEW.address_slot)
EXECUTE FUNCTION paperboat.publish_lease_inventory();

-- +goose Down
DROP TRIGGER control_sync_leases_publish_inventory_update ON paperboat.control_sync_leases;
DROP TRIGGER control_sync_leases_publish_inventory_insert ON paperboat.control_sync_leases;
DROP FUNCTION paperboat.publish_lease_inventory();
