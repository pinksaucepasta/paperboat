-- +goose NO TRANSACTION
-- +goose Up
-- The per-account owner of a host key; replaces the global primary key on fingerprint (next migration).
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS machine_ssh_host_key_owners_account_fingerprint
    ON paperboat.machine_ssh_host_key_owners (account_id, fingerprint);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.machine_ssh_host_key_owners_account_fingerprint;
