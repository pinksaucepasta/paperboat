-- +goose Up
-- Data only, after the global key is gone: a refusal recorded for keys another account's machine holds is no
-- longer a conflict. Same-account conflicts (cloned VMs) stay; batches of 1000.
-- +goose StatementBegin
DO $$
DECLARE n integer;
BEGIN
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
DO $$ BEGIN RAISE EXCEPTION 'cross-account host-key conflict cleanup is forward-only'; END $$;
-- +goose StatementEnd
