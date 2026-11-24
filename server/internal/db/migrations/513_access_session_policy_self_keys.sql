-- +goose Up
-- An access session is read only by its own two endpoints' peer documents (peersessions/network.go grant query), but its
-- write published their bare ids, which are peer keys of every document of the account (prod 2026-11-25: ~31 session
-- writes/min in a 310-peer account). Expand step: a session write names 'self:<endpoint id>' (a key only the endpoint's
-- own document holds, policy_cache.go policyKeys) as well as the bare ids, which the previous server still needs during
-- a rollout or after a rollback. The contract step drops the bare ids once no previous server can serve.
-- Function bodies only; no table, trigger or data changes.
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);

-- +goose StatementBegin
-- publish_machine_topic('machine_access') for a session write; the contract step stops naming the machine to the cache.
CREATE FUNCTION paperboat.publish_access_session_topic(machine_ids text[]) RETURNS void
    LANGUAGE sql AS $$ SELECT paperboat.publish_machine_topic(machine_ids, 'machine_access') $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_access_sessions() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM paperboat.publish_access_session_topic(ARRAY(SELECT DISTINCT user_machine_id FROM new_sessions));
  ELSE
    PERFORM paperboat.publish_access_session_topic(ARRAY(SELECT DISTINCT c.user_machine_id FROM (
      SELECT id, user_machine_id, state FROM new_sessions EXCEPT SELECT id, user_machine_id, state FROM old_sessions) c));
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_peer_policy_access_sessions() RETURNS trigger LANGUAGE plpgsql AS $$
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
      LATERAL unnest(ARRAY[c.user_machine_id, c.cli_client_session_id, 'self:' || c.user_machine_id, 'self:' || c.cli_client_session_id]) k));
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_peer_policy_account_row() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE r record;
BEGIN
  IF TG_OP = 'DELETE' THEN r := OLD; ELSE r := NEW; END IF;
  CASE TG_TABLE_NAME
  WHEN 'peer_network_identities' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY['account:' || r.user_id, r.endpoint_id, r.machine_id]);
  WHEN 'peer_endpoint_certificates' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY['account:' || r.user_id, r.endpoint_id]);
  WHEN 'cli_client_sessions' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY['account:' || r.user_id, r.id]);
  WHEN 'users' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY['account:' || r.id]);
  WHEN 'control_environments' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY['account:' || r.owner_user_id]
      || CASE WHEN TG_OP = 'UPDATE' THEN ARRAY['account:' || OLD.owner_user_id] ELSE '{}'::text[] END);
  WHEN 'user_machine_terminal_sessions' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY(
      SELECT k FROM paperboat.user_machine_access_sessions s, LATERAL unnest(ARRAY[s.cli_client_session_id, s.user_machine_id]) k
      WHERE s.terminal_session_id = r.id AND s.state = 'active'));
  WHEN 'control_config_assignments', 'control_config_sync_statuses' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY[r.machine_id]
      || ARRAY(SELECT 'account:' || e.owner_user_id FROM paperboat.control_environments e WHERE e.id = r.environment_id));
  WHEN 'billing_workspaces' THEN -- a team's paid entitlement (team_paid_entitled); personal relay quota is read per request
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_team_keys(ARRAY(SELECT x FROM unnest(ARRAY[r.team_id, CASE WHEN TG_OP = 'UPDATE' THEN OLD.team_id END]) x WHERE x IS NOT NULL)));
  WHEN 'recording_keys' THEN
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_team_keys(ARRAY[r.team_id]));
  -- Grants and admissions name the client endpoint and the machine, before and after; not their accounts, so an exec
  -- does not drop every other document of the account. A lazy policy has no client endpoint: its account.
  WHEN 'user_machine_access_sessions' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY[r.user_machine_id, r.cli_client_session_id, 'self:' || r.user_machine_id, 'self:' || r.cli_client_session_id]);
  WHEN 'inspector_credentials' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY[r.native_machine_id, r.native_cli_session_id]
      || CASE WHEN TG_OP = 'UPDATE' THEN ARRAY[OLD.native_machine_id, OLD.native_cli_session_id] ELSE '{}'::text[] END);
  WHEN 'config_compare_admissions', 'recording_native_admissions' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY[r.machine_id, r.native_cli_session_id]
      || CASE WHEN TG_OP = 'UPDATE' THEN ARRAY[OLD.machine_id, OLD.native_cli_session_id] ELSE '{}'::text[] END);
  WHEN 'lazy_access_policies' THEN
    PERFORM paperboat.publish_peer_policy(ARRAY['account:' || r.account_id, r.machine_id]
      || CASE WHEN TG_OP = 'UPDATE' THEN ARRAY['account:' || OLD.account_id, OLD.machine_id] ELSE '{}'::text[] END);
  END CASE;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose Down
-- Forward only (zero users).
SELECT 1;
