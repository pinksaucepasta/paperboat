-- +goose Up
-- The peer-network generation triggers selected an account's identities with
--   WHERE n.user_id = $1 AND n.endpoint_id = ANY (ARRAY(<subquery>))
-- The array is an InitPlan, whose size the planner cannot see, so it chose the user_id-only index with the endpoints
-- as a filter: every identity of the account was read and, under SERIALIZABLE, SIREAD-locked (3-4 page and tuple
-- locks for an account of 60 identities; the whole account at scale), and every pairing, seat or session change of
-- one machine conflicted with the others. The endpoints are now materialised first and the statement is planned
-- with their values (force_custom_plan; a generic plan has the same problem), so it probes the (user_id,
-- endpoint_id) primary key for exactly the identities it changes. Same bodies as migrations 297 and 131 otherwise.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_machine() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off SET plan_cache_mode = force_custom_plan AS $$
DECLARE
  fence boolean := NOT (OLD.state IN ('online','offline') AND NEW.state IN ('online','offline'))
    OR ROW(OLD.seat_state, OLD.revoked_at, OLD.deleted_at, OLD.installation_generation)
       IS DISTINCT FROM ROW(NEW.seat_state, NEW.revoked_at, NEW.deleted_at, NEW.installation_generation);
  ids text[] := ARRAY(
    SELECT NEW.id
    UNION SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.id);
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = CASE WHEN fence AND n.endpoint_id = NEW.id THEN n.config_generation + 1 ELSE n.relay_revocation_generation END,
      config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id = ANY (ids);
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_cli_session() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off SET plan_cache_mode = force_custom_plan AS $$
DECLARE
  ids text[] := ARRAY(
    SELECT NEW.id
    UNION SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.id);
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = CASE WHEN n.endpoint_id = NEW.id THEN n.config_generation + 1 ELSE n.relay_revocation_generation END,
      config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id = ANY (ids);
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_certificate() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off SET plan_cache_mode = force_custom_plan AS $$
DECLARE
  ids text[] := ARRAY(
    SELECT NEW.endpoint_id
    UNION SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.endpoint_id
    UNION SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.endpoint_id);
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = CASE WHEN n.endpoint_id = NEW.endpoint_id THEN n.config_generation + 1 ELSE n.relay_revocation_generation END,
      config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id = ANY (ids);
  RETURN NEW;
END $$;

-- The changed identity itself is fenced by fence_peer_network_local_identity; its counterparts only reconfigure.
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_identity() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off SET plan_cache_mode = force_custom_plan AS $$
DECLARE
  ids text[] := ARRAY(
    SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.endpoint_id
    UNION SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.endpoint_id);
BEGIN
  UPDATE peer_network_identities n
  SET config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id <> NEW.endpoint_id AND n.endpoint_id = ANY (ids);
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_environment() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off SET plan_cache_mode = force_custom_plan AS $$
DECLARE
  ids text[] := ARRAY(
    SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.owner_user_id AND a.environment_id = NEW.id
    UNION SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.owner_user_id AND a.environment_id = NEW.id);
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.owner_user_id AND n.endpoint_id = ANY (ids);
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION '460 is forward-only: restore the five advance_peer_network_for_* functions from migrations 297 and 131'; END $$;
-- +goose StatementEnd
