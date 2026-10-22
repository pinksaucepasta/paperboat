-- +goose Up
-- The peer-network triggers evaluated their condition for every peer identity of the account, so a serializable
-- transaction that fired one (pairing's seat update, a revoke, a renewal) read-locked the account's whole identity
-- set, and every daemon's concurrent peer-config refresh made it the pivot of a serialization failure: pairing
-- failed all ten retries (500 after 9.3 s). They now compute the exact endpoints first and update those rows by
-- primary key, so they lock only what they change. The rows updated are the same. These tables are small, and on a
-- small table the planner prefers a sequential scan, which under SERIALIZABLE takes a predicate lock on the whole
-- table; enable_seqscan = off keeps the triggers on their indexes.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_machine() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id = ANY (ARRAY(
    SELECT NEW.id
    UNION SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.id));
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_cli_session() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id = ANY (ARRAY(
    SELECT NEW.id
    UNION SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.id));
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_certificate() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id = ANY (ARRAY(
    SELECT NEW.endpoint_id
    UNION SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.endpoint_id
    UNION SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.endpoint_id));
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_identity() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id <> NEW.endpoint_id AND n.endpoint_id = ANY (ARRAY(
    SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.endpoint_id
    UNION SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.endpoint_id));
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_environment() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.owner_user_id AND n.endpoint_id = ANY (ARRAY(
    SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.owner_user_id AND a.environment_id = NEW.id
    UNION SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.owner_user_id AND a.environment_id = NEW.id));
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_machine() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND (n.endpoint_id = NEW.id OR EXISTS (
    SELECT 1 FROM user_machine_access_sessions a
    WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.id
      AND a.cli_client_session_id = n.endpoint_id));
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_cli_session() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND (n.endpoint_id = NEW.id OR EXISTS (
    SELECT 1 FROM user_machine_access_sessions a
    WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.id
      AND a.user_machine_id = n.endpoint_id));
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_certificate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND (n.endpoint_id = NEW.endpoint_id OR EXISTS (
    SELECT 1 FROM user_machine_access_sessions a
    WHERE a.user_id = NEW.user_id AND
      ((a.cli_client_session_id = NEW.endpoint_id AND a.user_machine_id = n.endpoint_id) OR
       (a.user_machine_id = NEW.endpoint_id AND a.cli_client_session_id = n.endpoint_id))));
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id <> NEW.endpoint_id AND EXISTS (
    SELECT 1 FROM user_machine_access_sessions a
    WHERE a.user_id = NEW.user_id AND
      ((a.cli_client_session_id = NEW.endpoint_id AND a.user_machine_id = n.endpoint_id) OR
       (a.user_machine_id = NEW.endpoint_id AND a.cli_client_session_id = n.endpoint_id)));
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_environment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.owner_user_id AND EXISTS (
    SELECT 1 FROM user_machine_access_sessions a
    WHERE a.user_id = NEW.owner_user_id AND a.environment_id = NEW.id
      AND (a.cli_client_session_id = n.endpoint_id OR a.user_machine_id = n.endpoint_id));
  RETURN NEW;
END $$;
-- +goose StatementEnd
