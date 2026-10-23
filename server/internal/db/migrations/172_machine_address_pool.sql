-- +goose Up
-- A machine's local address lease was one slot of a platform-wide pool of 64,770 (127.100.0.0/16): about 64.8k
-- machines ever, then pairing failed. The pool is now 127.64.0.0/10 (PLAN 3b "Machine local addresses", Go
-- machineaddress.FromSlot): slot s is 127.64.0.0 + ((s + 36*65536) mod 2^22), so slots below 65,536 keep their
-- 127.100.x.y address and a server still on the old formula reads them right. Slots are handed out after the
-- highest one in use, so 127.100/16 fills first (released daemons accept only it). A deleted machine's address is
-- not handed out again for the pool's quarantine (7 days), so a host that still maps the old name cannot reach a
-- different machine at it.
-- Prior art: Tailscale gives each node one address from 100.64.0.0/10 and keeps it when the node is shared;
-- headscale allocates in one prefix and frees on delete (hscontrol/db/ip.go IPAllocator).

-- The one place that says which slots may be leased: inside the /10, never last octet 0 or 255, never
-- 127.100.0.0 or the browser gateway 127.100.0.1 (slots 0 and 1). Outside the /10 is never reachable, so
-- 127.0.0.0/16 (127.0.0.1, 127.0.0.53, 127.0.1.1) is never handed out.
-- +goose StatementBegin
CREATE FUNCTION paperboat.machine_address_slot_allowed(slot integer) RETURNS boolean
    LANGUAGE sql IMMUTABLE AS $$
 SELECT slot >= 2 AND slot < 4194304 AND slot % 256 BETWEEN 1 AND 254;
$$;
-- +goose StatementEnd

CREATE TABLE paperboat.machine_address_pool (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    size integer NOT NULL CHECK (size BETWEEN 65536 AND 4194304),
    quarantine interval NOT NULL CHECK (quarantine >= interval '0')
);
INSERT INTO paperboat.machine_address_pool(size, quarantine) VALUES (4194304, interval '7 days');

ALTER TABLE paperboat.control_sync_leases DROP CONSTRAINT control_sync_leases_address_slot_check;
ALTER TABLE paperboat.control_sync_leases ADD CONSTRAINT control_sync_leases_address_slot_check
    CHECK (paperboat.machine_address_slot_allowed(address_slot));
ALTER TABLE paperboat.control_sync_leases ADD COLUMN released_at timestamptz;
-- Leases released before this migration have no release time; their quarantine starts now.
UPDATE paperboat.control_sync_leases SET released_at = now() WHERE machine_id IS NULL;
-- small-table: one row per machine ever, read only for released rows.
CREATE INDEX control_sync_leases_released ON paperboat.control_sync_leases (released_at, address_slot) WHERE machine_id IS NULL;

-- The machine delete's ON DELETE SET NULL (and any other release) stamps the release time.
-- +goose StatementBegin
CREATE FUNCTION paperboat.stamp_control_sync_lease_release() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.machine_id IS NULL AND OLD.machine_id IS NOT NULL THEN
    NEW.released_at := now();
  ELSIF NEW.machine_id IS NOT NULL THEN
    NEW.released_at := NULL;
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER control_sync_leases_release BEFORE UPDATE OF machine_id ON paperboat.control_sync_leases
    FOR EACH ROW EXECUTE FUNCTION paperboat.stamp_control_sync_lease_release();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.ensure_control_sync_lease(requested_machine text) RETURNS TABLE(address_slot integer, tags jsonb)
    LANGUAGE plpgsql
    AS $$
DECLARE
 pool paperboat.machine_address_pool;
 highest integer;
 candidate integer;
BEGIN
 RETURN QUERY SELECT l.address_slot,l.tags FROM paperboat.control_sync_leases l WHERE l.machine_id=requested_machine;
 IF FOUND THEN RETURN; END IF;
 SELECT * INTO pool FROM paperboat.machine_address_pool;
 -- A released slot whose quarantine is over.
 RETURN QUERY UPDATE paperboat.control_sync_leases l SET machine_id=requested_machine,tags='[]'::jsonb
  WHERE l.address_slot=(SELECT f.address_slot FROM paperboat.control_sync_leases f
   WHERE f.machine_id IS NULL AND f.released_at < now() - pool.quarantine AND f.address_slot < pool.size
   ORDER BY f.released_at, f.address_slot LIMIT 1 FOR UPDATE SKIP LOCKED)
  RETURNING l.address_slot,l.tags;
 IF FOUND THEN RETURN; END IF;
 -- The next slots after the highest in use. One a concurrent allocation is inserting right now is skipped (its
 -- advisory lock is held until that transaction ends), so INSERT ... ON CONFLICT never waits for it; when others
 -- took the whole window first, the highest is read again (bounded).
 FOR round IN 1..8 LOOP
  SELECT coalesce(max(l.address_slot), 1) INTO highest FROM paperboat.control_sync_leases l;
  candidate := NULL;
  FOR candidate IN SELECT n FROM generate_series(highest + 1, least(highest + 1024, pool.size - 1)) AS n
   WHERE paperboat.machine_address_slot_allowed(n) ORDER BY n LIMIT 64 LOOP
   CONTINUE WHEN NOT pg_try_advisory_xact_lock(hashtextextended('control_sync_slot:'||candidate, 0));
   RETURN QUERY INSERT INTO paperboat.control_sync_leases(machine_id,address_slot) VALUES(requested_machine,candidate)
    ON CONFLICT DO NOTHING RETURNING control_sync_leases.address_slot,control_sync_leases.tags;
   IF FOUND THEN RETURN; END IF;
   -- The slot was taken since the read, or this machine got a lease from a concurrent call.
   RETURN QUERY SELECT l.address_slot,l.tags FROM paperboat.control_sync_leases l WHERE l.machine_id=requested_machine;
   IF FOUND THEN RETURN; END IF;
  END LOOP;
  IF candidate IS NULL THEN RAISE EXCEPTION 'control sync address pool exhausted' USING ERRCODE='54000'; END IF;
 END LOOP;
 RAISE EXCEPTION 'control sync address allocation kept colliding' USING ERRCODE='40001';
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.ensure_control_sync_lease(requested_machine text) RETURNS TABLE(address_slot integer, tags jsonb)
    LANGUAGE plpgsql
    AS $$
DECLARE candidate integer;
BEGIN
 RETURN QUERY SELECT l.address_slot,l.tags FROM paperboat.control_sync_leases l WHERE l.machine_id=requested_machine;
 IF FOUND THEN RETURN; END IF;
 RETURN QUERY UPDATE paperboat.control_sync_leases l SET machine_id=requested_machine,tags='[]'::jsonb
  WHERE l.address_slot=(SELECT f.address_slot FROM paperboat.control_sync_leases f WHERE f.machine_id IS NULL
   ORDER BY f.address_slot LIMIT 1 FOR UPDATE SKIP LOCKED)
  RETURNING l.address_slot,l.tags;
 IF FOUND THEN RETURN; END IF;
 FOR candidate IN SELECT n FROM generate_series(2,65534) AS n
  WHERE n % 256 BETWEEN 1 AND 254 AND NOT EXISTS(SELECT 1 FROM paperboat.control_sync_leases l WHERE l.address_slot=n)
  ORDER BY n LIMIT 64 LOOP
  CONTINUE WHEN NOT pg_try_advisory_xact_lock(hashtextextended('control_sync_slot:'||candidate, 0));
  RETURN QUERY INSERT INTO paperboat.control_sync_leases(machine_id,address_slot) VALUES(requested_machine,candidate)
   ON CONFLICT DO NOTHING RETURNING control_sync_leases.address_slot,control_sync_leases.tags;
  IF FOUND THEN RETURN; END IF;
  RETURN QUERY SELECT l.address_slot,l.tags FROM paperboat.control_sync_leases l WHERE l.machine_id=requested_machine;
  IF FOUND THEN RETURN; END IF;
 END LOOP;
 IF candidate IS NULL THEN RAISE EXCEPTION 'control sync address pool exhausted' USING ERRCODE='54000'; END IF;
 RAISE EXCEPTION 'control sync address allocation kept colliding' USING ERRCODE='40001';
END;
$$;
-- +goose StatementEnd
DROP TRIGGER control_sync_leases_release ON paperboat.control_sync_leases;
DROP FUNCTION paperboat.stamp_control_sync_lease_release();
DROP INDEX paperboat.control_sync_leases_released;
ALTER TABLE paperboat.control_sync_leases DROP COLUMN released_at;
ALTER TABLE paperboat.control_sync_leases DROP CONSTRAINT control_sync_leases_address_slot_check;
ALTER TABLE paperboat.control_sync_leases ADD CONSTRAINT control_sync_leases_address_slot_check
    CHECK (address_slot >= 2 AND address_slot <= 65534 AND address_slot % 256 BETWEEN 1 AND 254);
DROP TABLE paperboat.machine_address_pool;
DROP FUNCTION paperboat.machine_address_slot_allowed(integer);
