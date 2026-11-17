-- +goose Up
-- Data only. Tunnel config snapshots are stored byte for byte and immutable, and the agent parses the served one
-- strictly. Those written before 285_route_tls_ca carry the retired route key "ca_reference":null, which the
-- agent no longer accepts. Each live tunnel whose current snapshot still has it gets a new pending generation
-- (sync activates the newest pending one, as for any route change) with the key removed; history stays as it is.
-- Batches of at most 500 tunnels; a tunnel is done once its current snapshot has no key, so a rerun does nothing.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
DO $$
DECLARE moved integer;
BEGIN
  LOOP
    WITH batch AS (
      SELECT t.id, t.generation, g.snapshot, g.created_by_actor_id
      FROM paperboat.tunnels t
      JOIN paperboat.tunnel_config_generations g ON g.tunnel_id = t.id AND g.generation = t.generation
      WHERE t.deleted_at IS NULL AND position('"ca_reference":null,' in convert_from(g.snapshot, 'UTF8')) > 0
      ORDER BY t.id LIMIT 500 FOR UPDATE OF t
    ), fixed AS (
      SELECT id, generation, created_by_actor_id,
             convert_to(replace(replace(convert_from(snapshot, 'UTF8'), '"ca_reference":null,', ''),
                                '"generation":' || generation || ',', '"generation":' || (generation + 1) || ','), 'UTF8') AS snapshot
      FROM batch
    ), added AS (
      INSERT INTO paperboat.tunnel_config_generations
        (tunnel_id, generation, previous_generation, content_hash, snapshot, activation_state, created_by_actor_id, created_at, retained_until)
      SELECT id, generation + 1, generation, sha256(snapshot), snapshot, 'pending', created_by_actor_id, now(), now() + interval '90 days'
      FROM fixed
      RETURNING tunnel_id
    )
    UPDATE paperboat.tunnels t SET generation = generation + 1, updated_at = now()
    FROM added WHERE t.id = added.tunnel_id;
    GET DIAGNOSTICS moved = ROW_COUNT;
    EXIT WHEN moved = 0;
  END LOOP;
  IF EXISTS (SELECT 1 FROM paperboat.tunnels t JOIN paperboat.tunnel_config_generations g ON g.tunnel_id = t.id AND g.generation = t.generation
             WHERE t.deleted_at IS NULL AND position('"ca_reference"' in convert_from(g.snapshot, 'UTF8')) > 0) THEN
    RAISE EXCEPTION 'a live tunnel snapshot still carries ca_reference (not in the canonical compact form)';
  END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'tunnel snapshot rewrite is forward-only'; END $$;
-- +goose StatementEnd
