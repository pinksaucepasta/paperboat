-- +goose Up
-- Step 1 of making host-key ownership per account (clone detection is for one account's own machines; one
-- computer with two OS users paired to two accounts presents one key set to both). The owning account is the
-- machine's user_id (immutable), not owner_team_id, which a machine can change after enrollment.
-- Expand only: the running server keeps inserting owners without account_id, so the trigger fills it, and
-- nothing the running server uses is dropped or tightened here. The global key is dropped in a later deploy.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.machine_ssh_host_key_owners ADD COLUMN account_id text;
-- +goose StatementBegin
CREATE FUNCTION paperboat.machine_ssh_host_key_owner_account() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.account_id IS NULL THEN
    SELECT user_id INTO NEW.account_id FROM paperboat.user_machines WHERE id = NEW.user_machine_id;
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER machine_ssh_host_key_owner_account BEFORE INSERT ON paperboat.machine_ssh_host_key_owners
    FOR EACH ROW EXECUTE FUNCTION paperboat.machine_ssh_host_key_owner_account();

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'host-key owner account is forward-only'; END $$;
-- +goose StatementEnd
