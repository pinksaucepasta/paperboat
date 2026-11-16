-- +goose Up
SET LOCAL lock_timeout = '5s';
-- A terminal session row is read by a peer policy document only through terminal_session_role() on an active access
-- session that names it (peersessions/network.go grant query), so a change names that access session's client
-- endpoint and machine, as publish_peer_policy_account_row does for user_machine_access_sessions. It named the owner's
-- account and the terminal's machine, which dropped every document of the account (each personal-mesh endpoint has
-- the machine and account as peer keys): prod 2026-11-16, a terminal test opening and closing sessions ~40/min kept a
-- 209-endpoint account at a near-zero cache hit rate, rebuilding ~1 s documents on every renewal.
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
    PERFORM paperboat.publish_peer_policy(ARRAY[r.user_machine_id, r.cli_client_session_id]
      || CASE WHEN TG_OP = 'UPDATE' THEN ARRAY[OLD.user_machine_id, OLD.cli_client_session_id] ELSE '{}'::text[] END);
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
SET LOCAL lock_timeout = '5s';
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
    PERFORM paperboat.publish_peer_policy(ARRAY[r.user_machine_id, 'account:' || r.owner_account]
      || ARRAY(SELECT 'account:' || s.user_id FROM paperboat.user_machine_access_sessions s WHERE s.terminal_session_id = r.id AND s.state = 'active'));
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
    PERFORM paperboat.publish_peer_policy(ARRAY[r.user_machine_id, r.cli_client_session_id]
      || CASE WHEN TG_OP = 'UPDATE' THEN ARRAY[OLD.user_machine_id, OLD.cli_client_session_id] ELSE '{}'::text[] END);
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
