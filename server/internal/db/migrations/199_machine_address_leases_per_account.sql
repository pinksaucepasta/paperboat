-- +goose Up
-- contract-after: 184
-- Machine local addresses are per account (PLAN 3b "Multi-account hosts", release B, the contract step after 184):
-- a lease is one (account, machine) with a slot that is unique only inside its account, in 127.100/16 (slots 2..65534,
-- last octet 1..254). The global pool is gone: the slot primary key and unique(machine_id) (two accounts may hold the
-- same slot, and one machine has a lease per account that can see it), the /10 pool table and its quarantine, released
-- rows and their release stamp (a deleted machine's leases are deleted with it), the sync tags (no reader left since
-- the control sync server was deleted), and the global allocator ensure_control_sync_lease. One allocator remains,
-- ensure_machine_address_leases(account, machines): set-based, serialized per account by an advisory lock, lowest free
-- slots of that account; nothing takes a table lock.
-- Prior art: Tailscale hands each node one address from 100.64.0.0/10 and keeps it when shared; here each account has
-- its own 127.100/16 because the addresses are local to a computer, and the host guard resolves clashes between the
-- accounts of one computer.

-- Released rows (quarantine) and rows without an owner account go.
DELETE FROM paperboat.control_sync_leases WHERE machine_id IS NULL OR account_id IS NULL;

ALTER TABLE paperboat.control_sync_leases ALTER COLUMN machine_id SET NOT NULL;
ALTER TABLE paperboat.control_sync_leases ALTER COLUMN account_id SET NOT NULL;
ALTER TABLE paperboat.control_sync_leases DROP CONSTRAINT control_sync_leases_pkey;
ALTER TABLE paperboat.control_sync_leases DROP CONSTRAINT control_sync_leases_machine_id_key;
ALTER TABLE paperboat.control_sync_leases ADD PRIMARY KEY USING INDEX control_sync_leases_account_machine;
ALTER TABLE paperboat.control_sync_leases DROP CONSTRAINT control_sync_leases_machine_id_fkey;
ALTER TABLE paperboat.control_sync_leases ADD CONSTRAINT control_sync_leases_machine_id_fkey
    FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;

DROP TRIGGER control_sync_leases_release ON paperboat.control_sync_leases;
DROP FUNCTION paperboat.stamp_control_sync_lease_release();
DROP INDEX paperboat.control_sync_leases_released;
ALTER TABLE paperboat.control_sync_leases DROP COLUMN released_at;
ALTER TABLE paperboat.control_sync_leases DROP COLUMN tags;
DROP FUNCTION paperboat.ensure_control_sync_lease(text);
ALTER TABLE paperboat.control_sync_leases DROP CONSTRAINT control_sync_leases_address_slot_check;
DROP FUNCTION paperboat.machine_address_slot_allowed(integer);
ALTER TABLE paperboat.control_sync_leases ADD CONSTRAINT control_sync_leases_address_slot_check
    CHECK (address_slot BETWEEN 2 AND 65534 AND address_slot % 256 BETWEEN 1 AND 254);
DROP TABLE paperboat.machine_address_pool;

-- A lease belongs to one account, so only that account's topics move (before: the machine's whole audience, because
-- the address was shared).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_lease_inventory() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY[NEW.account_id], 'inventory');
  PERFORM paperboat.publish_account_topic(ARRAY[NEW.account_id], 'peer_network');
  RETURN NEW;
END $$;
-- +goose StatementEnd
DROP TRIGGER control_sync_leases_publish_inventory_insert ON paperboat.control_sync_leases;
DROP TRIGGER control_sync_leases_publish_inventory_update ON paperboat.control_sync_leases;
CREATE TRIGGER control_sync_leases_publish_inventory_insert AFTER INSERT ON paperboat.control_sync_leases
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_lease_inventory();
CREATE TRIGGER control_sync_leases_publish_inventory_update AFTER UPDATE OF machine_id, address_slot ON paperboat.control_sync_leases
    FOR EACH ROW WHEN (OLD.machine_id IS DISTINCT FROM NEW.machine_id OR OLD.address_slot IS DISTINCT FROM NEW.address_slot)
    EXECUTE FUNCTION paperboat.publish_lease_inventory();

-- The account's machines get the lowest free slots of that account in one statement; machines that already have one
-- keep it. 54000 (program_limit_exceeded) when the account has no more free addresses. Callers authorize first: the
-- machines are ones the account may see.
-- +goose StatementBegin
CREATE FUNCTION paperboat.ensure_machine_address_leases(account text, machine_ids text[])
    RETURNS TABLE(leased_machine text, leased_slot integer) LANGUAGE plpgsql AS $$
DECLARE
 missing text[];
 free integer[];
BEGIN
 PERFORM pg_advisory_xact_lock(hashtextextended('machine_address:' || account, 0));
 missing := ARRAY(SELECT m.id FROM paperboat.user_machines m WHERE m.id = ANY (machine_ids)
   AND NOT EXISTS (SELECT 1 FROM paperboat.control_sync_leases l WHERE l.account_id = account AND l.machine_id = m.id) ORDER BY m.id);
 IF cardinality(missing) > 0 THEN
  -- The lowest free slots lie below the highest one in use plus what is needed (two numbers per 256 are never
  -- leased); scanning the whole 127.100/16 for every pairing cost 24 ms under the account's lock.
  free := ARRAY(SELECT s FROM generate_series(2, least(65534, (SELECT coalesce(max(l.address_slot), 1) FROM paperboat.control_sync_leases l
      WHERE l.account_id = account) + 2 * cardinality(missing) + 4)) s WHERE s % 256 BETWEEN 1 AND 254
    AND NOT EXISTS (SELECT 1 FROM paperboat.control_sync_leases l WHERE l.account_id = account AND l.address_slot = s)
    ORDER BY s LIMIT cardinality(missing));
  IF cardinality(free) < cardinality(missing) THEN
   RAISE EXCEPTION 'machine address pool of the account is exhausted' USING ERRCODE = '54000';
  END IF;
  INSERT INTO paperboat.control_sync_leases(account_id, machine_id, address_slot)
   SELECT account, missing[i], free[i] FROM generate_subscripts(missing, 1) i;
 END IF;
 RETURN QUERY SELECT l.machine_id, l.address_slot FROM paperboat.control_sync_leases l
  WHERE l.account_id = account AND l.machine_id = ANY (machine_ids);
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'per-account machine address leases are irreversible (slots repeat across accounts); roll back the release instead';
END $$;
-- +goose StatementEnd
