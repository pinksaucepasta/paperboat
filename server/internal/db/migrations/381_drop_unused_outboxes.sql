-- +goose Up
-- helper_revocation_outbox and provider_route_cleanup_outbox (project/helper stack) have no reader or writer left;
-- a table nothing writes needs no retention. No other table references them.
SET LOCAL lock_timeout = '5s';
DROP TABLE IF EXISTS paperboat.helper_revocation_outbox;
DROP TABLE IF EXISTS paperboat.provider_route_cleanup_outbox;

-- +goose Down
SELECT 1;
