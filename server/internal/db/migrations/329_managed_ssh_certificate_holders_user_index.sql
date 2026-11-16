-- +goose NO TRANSACTION
-- +goose Up
-- The team-membership trigger (325) finds a removed member's certificate holders by user_id: without this index every
-- membership change scans the whole table.
SET lock_timeout = '5s';
-- A failed concurrent build leaves an INVALID index that IF NOT EXISTS would keep: drop any leftover, then build.
DROP INDEX CONCURRENTLY IF EXISTS paperboat.managed_ssh_certificate_holders_user;
CREATE INDEX CONCURRENTLY managed_ssh_certificate_holders_user ON paperboat.managed_ssh_certificate_holders (user_id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.managed_ssh_certificate_holders_user;
