-- +goose Up
-- Host-key ownership is per account from here on: account_id is required and the global primary key on
-- fingerprint and the unique on public_key go (the per-account unique index from 240 replaces them).
-- (fingerprint, user_machine_id) stays unique for the machine_ssh_host_keys foreign key. The fill trigger from
-- 238 stays one more deploy; the previous server inserts without account_id.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.machine_ssh_host_key_owners ADD CONSTRAINT machine_ssh_host_key_owners_account_set CHECK (account_id IS NOT NULL) NOT VALID;
ALTER TABLE paperboat.machine_ssh_host_key_owners VALIDATE CONSTRAINT machine_ssh_host_key_owners_account_set;
ALTER TABLE paperboat.machine_ssh_host_key_owners ALTER COLUMN account_id SET NOT NULL;
ALTER TABLE paperboat.machine_ssh_host_key_owners DROP CONSTRAINT machine_ssh_host_key_owners_account_set;
ALTER TABLE paperboat.machine_ssh_host_key_owners DROP CONSTRAINT machine_ssh_host_key_owners_pkey;
ALTER TABLE paperboat.machine_ssh_host_key_owners DROP CONSTRAINT machine_ssh_host_key_owners_public_key_key;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'host-key owner per-account uniqueness is forward-only'; END $$;
-- +goose StatementEnd
