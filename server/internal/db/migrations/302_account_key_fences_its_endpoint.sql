-- +goose Up
-- Revoking one account E2EE key (a machine removed, a CLI session ended, a key replaced) advanced the relay fence of
-- every endpoint of the account, so the relay refused the grants all of the account's other machines still held.
-- With the client waiting for a newer grant, same-account peers were unreachable for 1.5 h (fleet outage
-- 2026-11-12 20:39Z). The key belongs to one endpoint: only that endpoint is fenced. Every endpoint of the account
-- still moves to a new configuration generation, as before. A root revocation (account reset) still fences all.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_account_key() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = CASE WHEN n.endpoint_id = NEW.cli_client_session_id OR n.endpoint_id = NEW.user_machine_id
        THEN n.config_generation + 1 ELSE n.relay_revocation_generation END,
      config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id;
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.advance_peer_network_for_account_key() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id;
  RETURN NEW;
END $$;
-- +goose StatementEnd
