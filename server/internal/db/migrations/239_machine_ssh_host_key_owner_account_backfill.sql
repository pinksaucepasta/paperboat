-- +goose Up
-- Data only: owners that existed before the trigger get their account, in batches of 1000.
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
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'host-key owner account backfill is forward-only'; END $$;
-- +goose StatementEnd
