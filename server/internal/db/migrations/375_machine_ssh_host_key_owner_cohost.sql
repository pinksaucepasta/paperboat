-- +goose NO TRANSACTION
-- +goose Up
-- Two OS users of one computer, both paired to one account, share its host keys: the per-account owner of a key is no
-- longer unique. The observe transaction (account advisory lock) allows a second owner only when both machines report
-- the same live os_boot_id; a cloned image booted elsewhere still refuses as "in use".
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.machine_ssh_host_key_owners_account_fingerprint_lookup;
CREATE INDEX CONCURRENTLY machine_ssh_host_key_owners_account_fingerprint_lookup
    ON paperboat.machine_ssh_host_key_owners (account_id, fingerprint);
DROP INDEX CONCURRENTLY IF EXISTS paperboat.machine_ssh_host_key_owners_account_fingerprint;

-- +goose Down
SET lock_timeout = '5s';
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS machine_ssh_host_key_owners_account_fingerprint
    ON paperboat.machine_ssh_host_key_owners (account_id, fingerprint);
DROP INDEX CONCURRENTLY IF EXISTS paperboat.machine_ssh_host_key_owners_account_fingerprint_lookup;
