-- +goose Up
-- A self-hosted-only workspace's route kept the customer alias as its host; it is the managed host now (422 made the
-- carrier valid only there), as Attach sets it, so no runtime carrier reads invalid until its daemon attaches again.
-- Data only, in its own migration: one row per machine, a small table.
SET LOCAL lock_timeout = '5s';
UPDATE paperboat.control_routes SET public_host=managed_public_host WHERE kind='runtime_https_wss' AND public_host<>managed_public_host;

-- +goose Down
-- The previous alias is not recoverable and the previous function is restored by 422's Down.
SELECT 1;
