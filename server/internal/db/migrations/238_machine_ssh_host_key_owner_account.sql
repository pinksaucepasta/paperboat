-- +goose Up
-- Host-key uniqueness becomes per account (clone detection is for one account's own machines): one physical
-- computer with two OS users paired to two accounts presents one key set to both. The owning account is the
-- machine's user_id (immutable), not owner_team_id, which a machine can change after enrollment.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.machine_ssh_host_key_owners ADD COLUMN account_id text;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'host-key owner account is forward-only'; END $$;
-- +goose StatementEnd
