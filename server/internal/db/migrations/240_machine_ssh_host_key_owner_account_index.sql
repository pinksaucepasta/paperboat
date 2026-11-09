-- +goose NO TRANSACTION
-- +goose Up
-- The per-account owner of a host key. Weaker than the global primary key still in place, so safe for the running server.
SET lock_timeout = '5s';
-- A failed concurrent build leaves an INVALID index that IF NOT EXISTS would keep: drop any leftover, then build.
DROP INDEX CONCURRENTLY IF EXISTS paperboat.machine_ssh_host_key_owners_account_fingerprint;
CREATE UNIQUE INDEX CONCURRENTLY machine_ssh_host_key_owners_account_fingerprint
    ON paperboat.machine_ssh_host_key_owners (account_id, fingerprint);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.machine_ssh_host_key_owners_account_fingerprint;
