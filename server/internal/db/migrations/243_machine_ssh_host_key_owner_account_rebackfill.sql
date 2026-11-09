-- +goose Up
-- Data only, right before NOT NULL: the fill trigger covers every insert since 238, so this finds nothing
-- unless a row slipped in without it; batches of 1000.
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
DO $$ BEGIN RAISE EXCEPTION 'host-key owner account rebackfill is forward-only'; END $$;
-- +goose StatementEnd
