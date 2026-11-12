-- +goose Up
-- A relay-only pair cut off from the control plane went dark about two minutes into the cut: the server marked the
-- silent machine offline, and the machine trigger advanced its relay revocation generation, so the relay fenced the
-- grant the machine still held ("fence (generation 52, held 57)"). The triggers did the same to every counterpart
-- of a changed endpoint (a CLI session's certificate renewal or key rotation fenced each machine it ever reached).
-- A relay fence is a revocation: it now moves only for the endpoint whose own right ended (removed, revoked,
-- reinstalled, its certificate or session ended). online<->offline and a peer's change move only the configuration
-- generation, so a cut-off endpoint keeps relaying on its last grant until it is revoked (the Tailscale model:
-- tailscale derp/derpserver/derpserver.go verifyClient admits any node key the tailnet still knows; liveness never revokes it). A
-- counterpart of a removed endpoint needs no fence: the removed endpoint is itself fenced off the relay.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_machine() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
DECLARE
  fence boolean := NOT (OLD.state IN ('online','offline') AND NEW.state IN ('online','offline'))
    OR ROW(OLD.seat_state, OLD.revoked_at, OLD.deleted_at, OLD.installation_generation)
       IS DISTINCT FROM ROW(NEW.seat_state, NEW.revoked_at, NEW.deleted_at, NEW.installation_generation);
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = CASE WHEN fence AND n.endpoint_id = NEW.id THEN n.config_generation + 1 ELSE n.relay_revocation_generation END,
      config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id = ANY (ARRAY(
    SELECT NEW.id
    UNION SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.id));
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_cli_session() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = CASE WHEN n.endpoint_id = NEW.id THEN n.config_generation + 1 ELSE n.relay_revocation_generation END,
      config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id = ANY (ARRAY(
    SELECT NEW.id
    UNION SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.id));
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_certificate() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = CASE WHEN n.endpoint_id = NEW.endpoint_id THEN n.config_generation + 1 ELSE n.relay_revocation_generation END,
      config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id = ANY (ARRAY(
    SELECT NEW.endpoint_id
    UNION SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.endpoint_id
    UNION SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.endpoint_id));
  RETURN NEW;
END $$;

-- The changed identity itself is fenced by fence_peer_network_local_identity; its counterparts only reconfigure.
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_identity() RETURNS trigger LANGUAGE plpgsql SET enable_seqscan = off AS $$
BEGIN
  UPDATE peer_network_identities n
  SET config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id <> NEW.endpoint_id AND n.endpoint_id = ANY (ARRAY(
    SELECT a.user_machine_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.endpoint_id
    UNION SELECT a.cli_client_session_id FROM user_machine_access_sessions a WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.endpoint_id));
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
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
-- +goose StatementEnd
