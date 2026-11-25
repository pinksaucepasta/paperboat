-- +goose Up
-- Every finished transfer or operation fenced the relay grants of both its endpoints (473's UPDATE branch moved
-- relay_revocation_generation for any change of an access session): the relays closed all their connections and refused the
-- grant they held until the next one ("fence (generation 193, held 194)"), so each `pb send` cut every relayed path of
-- both machines for 3-8 s and flipped their home_relay. A fence is a revocation (297): a session its own client reports
-- finished only reconfigures. Any other end or change (revoked, removed, expired) fences as before.
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
      SELECT n.user_id, n.cli_client_session_id, n.user_machine_id,
        bool_or((o.state = 'active' AND n.state = 'revoked'
          AND n.revocation_reason IN ('transfer_finished', 'operation_ended')) IS NOT TRUE) AS fence
      FROM new_sessions n JOIN old_sessions o USING (id)
      WHERE ROW(n.user_id, n.cli_client_session_id, n.user_machine_id, n.state, n.revoked_at, n.expires_at)
        IS DISTINCT FROM ROW(o.user_id, o.cli_client_session_id, o.user_machine_id, o.state, o.revoked_at, o.expires_at)
      GROUP BY n.user_id, n.cli_client_session_id, n.user_machine_id),
    ends AS (
      SELECT c.user_id, c.cli_client_session_id AS endpoint_id, c.fence FROM changed c
      UNION ALL
      SELECT m.user_id, m.id, c.fence FROM changed c JOIN paperboat.user_machines m ON m.id = c.user_machine_id),
    endpoints AS (SELECT user_id, endpoint_id, bool_or(fence) AS fence FROM ends GROUP BY user_id, endpoint_id),
    locked AS (
      SELECT n.user_id,n.endpoint_id,e.fence FROM paperboat.peer_network_identities n JOIN endpoints e USING(user_id,endpoint_id)
      ORDER BY n.user_id,n.endpoint_id FOR NO KEY UPDATE OF n)
    UPDATE paperboat.peer_network_identities n
    SET relay_revocation_generation = CASE WHEN e.fence THEN n.config_generation + 1 ELSE n.relay_revocation_generation END,
      config_generation = n.config_generation + 1, updated_at = now()
    FROM locked e WHERE n.user_id = e.user_id AND n.endpoint_id = e.endpoint_id;
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION '517 is forward-only: restore advance_peer_network_for_access_sessions from migration 473'; END $$;
-- +goose StatementEnd
