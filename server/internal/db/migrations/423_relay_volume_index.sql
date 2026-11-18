-- +goose NO TRANSACTION
-- +goose Up
-- A relay's observation asks how many managed-relay bytes each connected account carried this month (the fair-use speed tier);
-- without this partial index each lookup scans the account's whole window of usage rows.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS paperboat.resource_bandwidth_relay_actor_month;
CREATE INDEX CONCURRENTLY resource_bandwidth_relay_actor_month ON paperboat.resource_bandwidth_events (actor_account_id, interval_end) WHERE mode = 'relay' AND hosting = 'managed';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS paperboat.resource_bandwidth_relay_actor_month;
