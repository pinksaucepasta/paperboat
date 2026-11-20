-- +goose Up
-- Access sessions share their client and machine identities with other writers.
-- Lock those rows in primary key order before bumping their generations.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_access_sessions() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  -- Only the branch taken is planned: an insert trigger has no old table.
  IF TG_OP = 'INSERT' THEN
    WITH changed AS (SELECT DISTINCT n.user_id, n.cli_client_session_id, n.user_machine_id FROM new_sessions n),
    endpoints AS (
      SELECT c.user_id, c.cli_client_session_id AS endpoint_id FROM changed c
      UNION
      SELECT m.user_id, m.id FROM changed c JOIN paperboat.user_machines m ON m.id = c.user_machine_id),
    locked AS (
      SELECT n.user_id,n.endpoint_id FROM paperboat.peer_network_identities n JOIN endpoints e USING(user_id,endpoint_id)
      ORDER BY n.user_id,n.endpoint_id FOR NO KEY UPDATE OF n)
    UPDATE paperboat.peer_network_identities n SET config_generation = config_generation + 1, updated_at = now()
    FROM locked e WHERE n.user_id = e.user_id AND n.endpoint_id = e.endpoint_id;
  ELSE
    WITH changed AS (
      SELECT DISTINCT c.user_id, c.cli_client_session_id, c.user_machine_id FROM (
        SELECT id, user_id, cli_client_session_id, user_machine_id, state, revoked_at, expires_at FROM new_sessions
        EXCEPT SELECT id, user_id, cli_client_session_id, user_machine_id, state, revoked_at, expires_at FROM old_sessions) c),
    endpoints AS (
      SELECT c.user_id, c.cli_client_session_id AS endpoint_id FROM changed c
      UNION
      SELECT m.user_id, m.id FROM changed c JOIN paperboat.user_machines m ON m.id = c.user_machine_id),
    locked AS (
      SELECT n.user_id,n.endpoint_id FROM paperboat.peer_network_identities n JOIN endpoints e USING(user_id,endpoint_id)
      ORDER BY n.user_id,n.endpoint_id FOR NO KEY UPDATE OF n)
    UPDATE paperboat.peer_network_identities n SET relay_revocation_generation = config_generation + 1,
      config_generation = config_generation + 1, updated_at = now()
    FROM locked e WHERE n.user_id = e.user_id AND n.endpoint_id = e.endpoint_id;
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION '473 is forward-only: restore advance_peer_network_for_access_sessions from migration 443'; END $$;
-- +goose StatementEnd
