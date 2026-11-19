-- +goose Up
-- Prod 2026-11-19: revoking a machine with ~16k active (expired) access sessions ran 15 s in one statement. The cascade
-- revokes every session in one UPDATE, and per session (1) advance_peer_network_for_access_sessions matched
-- `id = ANY(array of every id)` and the statement triggers joined old_sessions to new_sessions by id, which the planner
-- runs as a nested loop (transition tables have no statistics: 25M comparisons, 7 s each for 5k sessions); (2)
-- native_usage_admission rewrote the usage authority although its value did not change; (3) peer_policy_update ran a
-- function and pg_notify per row. Now the changed rows are `new EXCEPT old` over the compared columns (hashed or
-- sorted, linear; EXCEPT treats NULLs as equal, as IS DISTINCT FROM does), (2) fires only when the authority's expiry
-- changes, (3) is one statement trigger publishing the distinct keys.
-- Catalog changes only (functions and triggers of user_machine_access_sessions); no data is rewritten.
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_access_sessions() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  -- Only the branch taken is planned: an insert trigger has no old table.
  IF TG_OP = 'INSERT' THEN
    WITH changed AS (SELECT DISTINCT n.user_id, n.cli_client_session_id, n.user_machine_id FROM new_sessions n),
    endpoints AS (
      SELECT c.user_id, c.cli_client_session_id AS endpoint_id FROM changed c
      UNION
      SELECT m.user_id, m.id FROM changed c JOIN paperboat.user_machines m ON m.id = c.user_machine_id)
    UPDATE paperboat.peer_network_identities n SET config_generation = config_generation + 1, updated_at = now()
    FROM endpoints e WHERE n.user_id = e.user_id AND n.endpoint_id = e.endpoint_id;
  ELSE
    WITH changed AS (
      SELECT DISTINCT c.user_id, c.cli_client_session_id, c.user_machine_id FROM (
        SELECT id, user_id, cli_client_session_id, user_machine_id, state, revoked_at, expires_at FROM new_sessions
        EXCEPT SELECT id, user_id, cli_client_session_id, user_machine_id, state, revoked_at, expires_at FROM old_sessions) c),
    endpoints AS (
      SELECT c.user_id, c.cli_client_session_id AS endpoint_id FROM changed c
      UNION
      SELECT m.user_id, m.id FROM changed c JOIN paperboat.user_machines m ON m.id = c.user_machine_id)
    UPDATE paperboat.peer_network_identities n SET relay_revocation_generation = config_generation + 1,
      config_generation = config_generation + 1, updated_at = now()
    FROM endpoints e WHERE n.user_id = e.user_id AND n.endpoint_id = e.endpoint_id;
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The keys publish_peer_policy_account_row names for one session (its machine and client endpoint, before and after),
-- for every changed session of the statement at once.
CREATE FUNCTION paperboat.publish_peer_policy_access_sessions() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  -- A changed row's new version is not in old_sessions and its old version not in new_sessions.
  PERFORM paperboat.publish_peer_policy(ARRAY(
    SELECT DISTINCT k FROM (
      (SELECT id, user_machine_id, user_id, cli_client_session_id, state, revoked_at, expires_at, capabilities, team_id, terminal_session_id,
        terminal_role, terminal_grant_generation, terminal_binding_generation, terminal_membership_generation, machine_service_port FROM new_sessions
       EXCEPT SELECT id, user_machine_id, user_id, cli_client_session_id, state, revoked_at, expires_at, capabilities, team_id, terminal_session_id,
        terminal_role, terminal_grant_generation, terminal_binding_generation, terminal_membership_generation, machine_service_port FROM old_sessions)
      UNION ALL
      (SELECT id, user_machine_id, user_id, cli_client_session_id, state, revoked_at, expires_at, capabilities, team_id, terminal_session_id,
        terminal_role, terminal_grant_generation, terminal_binding_generation, terminal_membership_generation, machine_service_port FROM old_sessions
       EXCEPT SELECT id, user_machine_id, user_id, cli_client_session_id, state, revoked_at, expires_at, capabilities, team_id, terminal_session_id,
        terminal_role, terminal_grant_generation, terminal_binding_generation, terminal_membership_generation, machine_service_port FROM new_sessions)) c,
      LATERAL unnest(ARRAY[c.user_machine_id, c.cli_client_session_id]) k));
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
-- A signed credential needs no helper call (the environment's revocation fences it, usermachines revokeCredentialSessions):
-- it is done at revocation, so revoking a machine leaves no row per session for the revocation worker to mark.
CREATE OR REPLACE FUNCTION paperboat.revoke_invalid_team_machine_access() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE affected_machine text; affected_team text;
BEGIN
 IF TG_TABLE_NAME='user_machines' THEN
  affected_machine := NEW.id;
 ELSE
  IF TG_OP='DELETE' THEN affected_team := OLD.team_id; ELSE affected_team := NEW.team_id; END IF;
 END IF;
 UPDATE user_machine_access_sessions a SET state='revoked',revoked_at=now(),updated_at=now(),revocation_reason='machine_permission_revoked',
  helper_revoked_at=CASE WHEN a.credential_kind='control_plane_signed' THEN now() ELSE a.helper_revoked_at END
 WHERE a.state='active' AND ((affected_machine IS NOT NULL AND a.user_machine_id=affected_machine) OR (affected_team IS NOT NULL AND a.team_id=affected_team)) AND (EXISTS(
  SELECT 1 FROM unnest(a.capabilities) capability
  WHERE CASE WHEN a.machine_service_port IS NOT NULL THEN NOT machine_service_session_allowed(a.id) WHEN a.team_id IS NOT NULL THEN NOT team_machine_capability_allowed(a.user_id,a.team_id,a.user_machine_id,capability)
   ELSE NOT EXISTS(SELECT 1 FROM user_machines m WHERE m.id=a.user_machine_id AND m.user_id=a.user_id AND m.owner_team_id IS NULL AND m.revoked_at IS NULL AND m.deleted_at IS NULL) END)
 OR EXISTS(SELECT 1 FROM user_machines m WHERE m.id=a.user_machine_id AND
  (m.state IN ('revoked','disconnected','deleted')
   OR (a.capabilities && ARRAY['terminal','exec'] AND NOT m.configured_capabilities @> ARRAY['terminal_host'])
   OR ('managed_ssh'=ANY(a.capabilities) AND NOT m.configured_capabilities @> ARRAY['ssh_host'])
   OR ('files'=ANY(a.capabilities) AND NOT m.configured_capabilities @> ARRAY['file_receive']))));
 RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_access_sessions() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM paperboat.publish_machine_topic(ARRAY(SELECT DISTINCT user_machine_id FROM new_sessions), 'machine_access');
  ELSE
    PERFORM paperboat.publish_machine_topic(ARRAY(SELECT DISTINCT c.user_machine_id FROM (
      SELECT id, user_machine_id, state FROM new_sessions EXCEPT SELECT id, user_machine_id, state FROM old_sessions) c), 'machine_access');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
DROP TRIGGER peer_policy_update ON paperboat.user_machine_access_sessions;
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.user_machine_access_sessions
 REFERENCING OLD TABLE AS old_sessions NEW TABLE AS new_sessions
 FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_peer_policy_access_sessions();

-- The authority's expiry is least(expires_at, revoked_at): revoking a session that already expired leaves it as it is.
DROP TRIGGER native_usage_admission ON paperboat.user_machine_access_sessions;
CREATE TRIGGER native_usage_admission AFTER INSERT ON paperboat.user_machine_access_sessions
 FOR EACH ROW EXECUTE FUNCTION paperboat.snapshot_native_usage_authority();
CREATE TRIGGER native_usage_admission_update AFTER UPDATE OF expires_at, revoked_at ON paperboat.user_machine_access_sessions FOR EACH ROW
 WHEN (OLD.expires_at IS DISTINCT FROM NEW.expires_at
   OR least(OLD.expires_at, coalesce(OLD.revoked_at, OLD.expires_at)) IS DISTINCT FROM least(NEW.expires_at, coalesce(NEW.revoked_at, NEW.expires_at)))
 EXECUTE FUNCTION paperboat.snapshot_native_usage_authority();

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
DROP TRIGGER native_usage_admission_update ON paperboat.user_machine_access_sessions;
DROP TRIGGER native_usage_admission ON paperboat.user_machine_access_sessions;
CREATE TRIGGER native_usage_admission AFTER INSERT OR UPDATE OF expires_at,revoked_at ON paperboat.user_machine_access_sessions
 FOR EACH ROW EXECUTE FUNCTION paperboat.snapshot_native_usage_authority();
DROP TRIGGER peer_policy_update ON paperboat.user_machine_access_sessions;
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.user_machine_access_sessions FOR EACH ROW
 WHEN ((OLD.user_machine_id, OLD.user_id, OLD.cli_client_session_id, OLD.state, OLD.revoked_at, OLD.expires_at, OLD.capabilities, OLD.team_id, OLD.terminal_session_id,
        OLD.terminal_role, OLD.terminal_grant_generation, OLD.terminal_binding_generation, OLD.terminal_membership_generation, OLD.machine_service_port)
   IS DISTINCT FROM (NEW.user_machine_id, NEW.user_id, NEW.cli_client_session_id, NEW.state, NEW.revoked_at, NEW.expires_at, NEW.capabilities, NEW.team_id, NEW.terminal_session_id,
        NEW.terminal_role, NEW.terminal_grant_generation, NEW.terminal_binding_generation, NEW.terminal_membership_generation, NEW.machine_service_port))
 EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();
DROP FUNCTION paperboat.publish_peer_policy_access_sessions();
