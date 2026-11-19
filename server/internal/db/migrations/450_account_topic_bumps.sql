-- +goose Up
-- An account topic's generation was one row that every machine event of the account updated in its own transaction
-- (publish_account_topic). Two serializable transactions that update the same row cannot both commit: a revoke lost
-- that race to every enrollment of its account, all 10 retries, and returned 40001 to the user (CI 2026-11-19, tiny
-- predicate-lock pool test, 1 run in 8). A publish now appends a bump row (inserts never conflict); a topic's
-- generation is its base row plus its bumps, and the usermachines worker folds bumps into the base row in bounded
-- batches (fold_account_topic_bumps), so the sum only grows. The table becomes account_topic_base and
-- account_topic_generations a view of base + bumps per account and topic, so every reader is unchanged (machineevents
-- changed, peersessions meshSignal); an account_id filter is pushed into both branches (both indexed).
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
ALTER TABLE paperboat.account_topic_generations RENAME TO account_topic_base;
CREATE TABLE paperboat.account_topic_bumps (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
    topic text NOT NULL CHECK (topic IN ('machine_access', 'peer_network', 'previews', 'tunnels', 'config', 'inventory', 'machine_services', 'private_routes'))
);
CREATE INDEX account_topic_bumps_account_topic ON paperboat.account_topic_bumps (account_id, topic);
CREATE VIEW paperboat.account_topic_generations AS
 SELECT account_id, topic, sum(generation)::bigint AS generation FROM (
  SELECT account_id, topic, generation FROM paperboat.account_topic_base
  UNION ALL
  SELECT account_id, topic, count(*) FROM paperboat.account_topic_bumps GROUP BY account_id, topic) parts
 GROUP BY account_id, topic;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_account_topic(account_ids text[], wanted_topic text) RETURNS void
    LANGUAGE sql AS $$
 WITH bumped AS (
  INSERT INTO paperboat.account_topic_bumps(account_id, topic)
  SELECT u.id, wanted_topic FROM paperboat.users u WHERE u.id = ANY (account_ids)
  RETURNING account_id)
 SELECT pg_notify('machine_events','account:'||account_id) FROM bumped;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Moves the oldest bumps into their base rows, one bounded batch; a topic's base + bumps total is unchanged.
CREATE FUNCTION paperboat.fold_account_topic_bumps(batch integer) RETURNS bigint
    LANGUAGE sql AS $$
 WITH moved AS (
  DELETE FROM paperboat.account_topic_bumps WHERE id IN (SELECT id FROM paperboat.account_topic_bumps ORDER BY id LIMIT batch)
  RETURNING account_id, topic),
 counts AS (SELECT account_id, topic, count(*) AS n FROM moved GROUP BY account_id, topic),
 folded AS (
  INSERT INTO paperboat.account_topic_base(account_id, topic, generation, updated_at)
  SELECT account_id, topic, n, now() FROM counts
  ON CONFLICT (account_id, topic) DO UPDATE SET generation = paperboat.account_topic_base.generation + EXCLUDED.generation, updated_at = now())
 SELECT coalesce(sum(n), 0)::bigint FROM counts;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION '450 is forward-only: fold the bumps, then restore publish_account_topic from migration 150'; END $$;
-- +goose StatementEnd
