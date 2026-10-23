-- +goose Up
-- A machine's local address lease (127.100.x.y) was allocated lazily inside the machine list query, under a
-- table-wide SHARE ROW EXCLUSIVE lock and a scan of every slot: the first list of a 100-machine account ran 29 s and
-- every other list of it waited on Lock:relation (Lane W staging). The list now only reads leases; a machine gets its
-- lease when it connects to control sync (sync authority). Allocation takes no table lock: it reuses a released slot
-- (SKIP LOCKED) or inserts the lowest free one, skipping a slot a concurrent allocation holds (advisory lock):
-- concurrent allocations never wait for each other.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.ensure_control_sync_lease(requested_machine text) RETURNS TABLE(address_slot integer, tags jsonb)
    LANGUAGE plpgsql
    AS $$
DECLARE candidate integer;
BEGIN
 RETURN QUERY SELECT l.address_slot,l.tags FROM paperboat.control_sync_leases l WHERE l.machine_id=requested_machine;
 IF FOUND THEN RETURN; END IF;
 -- A slot released when its machine was deleted (machine_id set to NULL).
 RETURN QUERY UPDATE paperboat.control_sync_leases l SET machine_id=requested_machine,tags='[]'::jsonb
  WHERE l.address_slot=(SELECT f.address_slot FROM paperboat.control_sync_leases f WHERE f.machine_id IS NULL
   ORDER BY f.address_slot LIMIT 1 FOR UPDATE SKIP LOCKED)
  RETURNING l.address_slot,l.tags;
 IF FOUND THEN RETURN; END IF;
 -- The lowest free slots; a slot another allocation is inserting right now is skipped (its advisory lock is held
 -- until that transaction ends), so INSERT ... ON CONFLICT never waits on an uncommitted insert of the same slot.
 FOR candidate IN SELECT n FROM generate_series(2,65534) AS n
  WHERE n % 256 BETWEEN 1 AND 254 AND NOT EXISTS(SELECT 1 FROM paperboat.control_sync_leases l WHERE l.address_slot=n)
  ORDER BY n LIMIT 64 LOOP
  CONTINUE WHEN NOT pg_try_advisory_xact_lock(hashtextextended('control_sync_slot:'||candidate, 0));
  RETURN QUERY INSERT INTO paperboat.control_sync_leases(machine_id,address_slot) VALUES(requested_machine,candidate)
   ON CONFLICT DO NOTHING RETURNING control_sync_leases.address_slot,control_sync_leases.tags;
  IF FOUND THEN RETURN; END IF;
  -- The slot was taken since the scan, or this machine got a lease from a concurrent call.
  RETURN QUERY SELECT l.address_slot,l.tags FROM paperboat.control_sync_leases l WHERE l.machine_id=requested_machine;
  IF FOUND THEN RETURN; END IF;
 END LOOP;
 IF candidate IS NULL THEN RAISE EXCEPTION 'control sync address pool exhausted' USING ERRCODE='54000'; END IF;
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
 -- Serialize only allocation, never hold this lock for the stream lifetime.
 LOCK TABLE paperboat.control_sync_leases IN SHARE ROW EXCLUSIVE MODE;
 RETURN QUERY SELECT l.address_slot,l.tags FROM paperboat.control_sync_leases l WHERE l.machine_id=requested_machine;
 IF FOUND THEN RETURN; END IF;
 SELECT n INTO candidate FROM generate_series(2,65534) AS n
 WHERE n % 256 BETWEEN 1 AND 254 AND NOT EXISTS(SELECT 1 FROM paperboat.control_sync_leases l WHERE l.address_slot=n)
 ORDER BY n LIMIT 1;
 IF candidate IS NULL THEN RAISE EXCEPTION 'control sync address pool exhausted' USING ERRCODE='54000'; END IF;
 RETURN QUERY INSERT INTO paperboat.control_sync_leases(machine_id,address_slot) VALUES(requested_machine,candidate)
 RETURNING control_sync_leases.address_slot,control_sync_leases.tags;
END;
$$;
-- +goose StatementEnd
