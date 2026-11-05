-- +goose Up
-- Grants, certificates and session state belong to the owner (a CLI session or a machine), so every peer-network
-- fence that matched an identity by endpoint_id now matches it by owner_endpoint_id: a grant change reaches every
-- generation of the session's daemon, and an ended session revokes all of them.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.peer_network_identities
    ADD CONSTRAINT peer_network_identities_owner_check
        CHECK (cli_client_session_id IS NULL OR role = 'cli' AND cli_client_session_id <> endpoint_id) NOT VALID,
    ADD CONSTRAINT peer_network_identities_revocation_reason_check
        CHECK (revocation_reason IS NULL OR revocation_reason IN ('withdrawn', 'generation_evicted', 'drain_expired')) NOT VALID;
CREATE INDEX peer_network_identities_owner ON paperboat.peer_network_identities (user_id, owner_endpoint_id);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.revoke_ended_cli_peer_identity() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  UPDATE paperboat.peer_network_identities SET revoked_at = now()
  WHERE role = 'cli' AND user_id = NEW.user_id AND owner_endpoint_id = NEW.id AND revoked_at IS NULL;
  RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_access_sessions() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  changed_ids text[];
BEGIN
  -- Only the branch taken is planned: an insert trigger has no old table.
  IF TG_OP = 'INSERT' THEN
    changed_ids := ARRAY(SELECT id FROM new_sessions);
  ELSE
    changed_ids := ARRAY(SELECT n.id FROM new_sessions n JOIN old_sessions o ON o.id = n.id
      WHERE o.state IS DISTINCT FROM n.state OR o.revoked_at IS DISTINCT FROM n.revoked_at OR o.expires_at IS DISTINCT FROM n.expires_at);
  END IF;
  IF cardinality(changed_ids) = 0 THEN
    RETURN NULL;
  END IF;
  WITH changed AS (
    SELECT n.user_id, n.cli_client_session_id, n.user_machine_id FROM new_sessions n WHERE n.id = ANY(changed_ids)),
  owners AS (
    SELECT c.user_id, c.cli_client_session_id AS owner_id FROM changed c
    UNION
    SELECT m.user_id, m.id FROM changed c JOIN paperboat.user_machines m ON m.id = c.user_machine_id)
  UPDATE paperboat.peer_network_identities n SET
    relay_revocation_generation = CASE WHEN TG_OP = 'INSERT' THEN relay_revocation_generation ELSE config_generation + 1 END,
    config_generation = config_generation + 1, updated_at = now()
  FROM owners e WHERE n.user_id = e.user_id AND n.owner_endpoint_id = e.owner_id;
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_certificate() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.owner_endpoint_id = ANY (ARRAY(
    SELECT NEW.endpoint_id
    UNION SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.endpoint_id
    UNION SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.endpoint_id));
  RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_cli_session() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.owner_endpoint_id = ANY (ARRAY(
    SELECT NEW.id
    UNION SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.id));
  RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_environment() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.owner_user_id AND n.owner_endpoint_id = ANY (ARRAY(
    SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.owner_user_id AND a.environment_id = NEW.id
    UNION SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.owner_user_id AND a.environment_id = NEW.id));
  RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_identity() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.owner_endpoint_id <> NEW.owner_endpoint_id AND n.owner_endpoint_id = ANY (ARRAY(
    SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.owner_endpoint_id
    UNION SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.owner_endpoint_id));
  RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_machine() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.owner_endpoint_id = ANY (ARRAY(
    SELECT NEW.id
    UNION SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.id));
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
