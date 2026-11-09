-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.machine_ssh_host_key_owners ADD CONSTRAINT machine_ssh_host_key_owners_account_set CHECK (account_id IS NOT NULL) NOT VALID;
ALTER TABLE paperboat.machine_ssh_host_key_owners VALIDATE CONSTRAINT machine_ssh_host_key_owners_account_set;
ALTER TABLE paperboat.machine_ssh_host_key_owners ALTER COLUMN account_id SET NOT NULL;
ALTER TABLE paperboat.machine_ssh_host_key_owners DROP CONSTRAINT machine_ssh_host_key_owners_account_set;
-- One computer's key may be owned once per account, not once globally. (fingerprint, user_machine_id) stays
-- unique for the machine_ssh_host_keys foreign key.
ALTER TABLE paperboat.machine_ssh_host_key_owners DROP CONSTRAINT machine_ssh_host_key_owners_pkey;
ALTER TABLE paperboat.machine_ssh_host_key_owners DROP CONSTRAINT machine_ssh_host_key_owners_public_key_key;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'host-key owner per-account uniqueness is forward-only'; END $$;
-- +goose StatementEnd
