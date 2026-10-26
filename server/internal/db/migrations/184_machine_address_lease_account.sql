-- +goose Up
-- Machine local addresses become per account (PLAN 3b "Multi-account hosts", point 1; user decision 2026-10-26):
-- a lease belongs to (viewer account, machine), its slot unique only inside the account. This is the expand step:
-- every lease gets the account it belongs to (today always the machine's owner) and the per-account unique keys;
-- the global ones stay until the contract step (contract-after this migration), so servers still running the old
-- code, which address a lease by its slot alone, stay right. The allocator records the owner account.
ALTER TABLE paperboat.control_sync_leases ADD COLUMN account_id text REFERENCES paperboat.users(id) ON DELETE CASCADE;
UPDATE paperboat.control_sync_leases l SET account_id = m.user_id FROM paperboat.user_machines m WHERE m.id = l.machine_id;
-- small-table: one row per machine ever.
CREATE UNIQUE INDEX control_sync_leases_account_machine ON paperboat.control_sync_leases (account_id, machine_id);
-- small-table: one row per machine ever.
CREATE UNIQUE INDEX control_sync_leases_account_slot ON paperboat.control_sync_leases (account_id, address_slot);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.ensure_control_sync_lease(requested_machine text) RETURNS TABLE(address_slot integer, tags jsonb)
    LANGUAGE plpgsql
    AS $$
DECLARE
 pool paperboat.machine_address_pool;
 highest integer;
 candidate integer;
 owner text;
BEGIN
 RETURN QUERY SELECT l.address_slot,l.tags FROM paperboat.control_sync_leases l WHERE l.machine_id=requested_machine;
 IF FOUND THEN RETURN; END IF;
 SELECT * INTO pool FROM paperboat.machine_address_pool;
 SELECT m.user_id INTO owner FROM paperboat.user_machines m WHERE m.id=requested_machine;
 -- A released slot whose quarantine is over.
 RETURN QUERY UPDATE paperboat.control_sync_leases l SET machine_id=requested_machine,account_id=owner,tags='[]'::jsonb
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
   RETURN QUERY INSERT INTO paperboat.control_sync_leases(machine_id,account_id,address_slot) VALUES(requested_machine,owner,candidate)
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
DROP INDEX paperboat.control_sync_leases_account_slot;
DROP INDEX paperboat.control_sync_leases_account_machine;
ALTER TABLE paperboat.control_sync_leases DROP COLUMN account_id;
