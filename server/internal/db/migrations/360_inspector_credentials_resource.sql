-- +goose NO TRANSACTION
-- +goose Up
-- The preview and tunnel peer-policy triggers (migrations 362-364) find the live inspector credentials of one resource.
SET lock_timeout = '5s';
-- A failed concurrent build leaves an INVALID index that IF NOT EXISTS would keep: drop any leftover, then build.
DROP INDEX CONCURRENTLY IF EXISTS paperboat.inspector_credentials_resource;
CREATE INDEX CONCURRENTLY inspector_credentials_resource ON paperboat.inspector_credentials (resource_id) WHERE revoked_at IS NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.inspector_credentials_resource;
