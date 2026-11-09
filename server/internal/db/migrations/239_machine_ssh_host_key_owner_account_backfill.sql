-- +goose Up
-- Data only. Owners get their account in batches of 1000; a refusal recorded for keys another account's machine
-- holds is no longer a conflict (the machine's next observation succeeds and removes any leftover row anyway).
-- +goose StatementBegin
DO $$
DECLARE n integer;
BEGIN
  LOOP
    UPDATE paperboat.machine_ssh_host_key_owners o SET account_id = m.user_id
    FROM paperboat.user_machines m
    WHERE m.id = o.user_machine_id AND o.account_id IS NULL
      AND o.ctid IN (SELECT ctid FROM paperboat.machine_ssh_host_key_owners WHERE account_id IS NULL LIMIT 1000);
    GET DIAGNOSTICS n = ROW_COUNT;
    EXIT WHEN n = 0;
  END LOOP;
  LOOP
    DELETE FROM paperboat.machine_ssh_host_key_conflicts c
    WHERE c.user_machine_id IN (
      SELECT c2.user_machine_id FROM paperboat.machine_ssh_host_key_conflicts c2
      JOIN paperboat.user_machines a ON a.id = c2.user_machine_id
      JOIN paperboat.user_machines b ON b.id = c2.other_machine_id
      WHERE a.user_id <> b.user_id LIMIT 1000);
    GET DIAGNOSTICS n = ROW_COUNT;
    EXIT WHEN n = 0;
  END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'host-key owner account backfill is forward-only'; END $$;
-- +goose StatementEnd
