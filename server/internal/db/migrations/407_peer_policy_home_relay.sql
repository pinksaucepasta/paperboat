-- +goose Up
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
-- A machine's home relay is part of the peer entry its peers' documents carry (406): a change names the same parties as
-- any other published column of the identity (351), instead of only the coarse account topic. Dropped and created in one
-- transaction, so no write goes by without it; no table rewrite, the lock is held for the two catalog changes only.
DROP TRIGGER peer_policy_update ON paperboat.peer_network_identities;
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.peer_network_identities FOR EACH ROW
 WHEN ((OLD.role, OLD.machine_id, OLD.endpoint_generation, OLD.machine_generation, OLD.key_generation, OLD.wireguard_public_key, OLD.disco_public_key,
        OLD.quic_certificate_fingerprint, OLD.virtual_address, OLD.published_endpoints, OLD.home_relay, OLD.revoked_at)
   IS DISTINCT FROM (NEW.role, NEW.machine_id, NEW.endpoint_generation, NEW.machine_generation, NEW.key_generation, NEW.wireguard_public_key, NEW.disco_public_key,
        NEW.quic_certificate_fingerprint, NEW.virtual_address, NEW.published_endpoints, NEW.home_relay, NEW.revoked_at))
 EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
DROP TRIGGER peer_policy_update ON paperboat.peer_network_identities;
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.peer_network_identities FOR EACH ROW
 WHEN ((OLD.role, OLD.machine_id, OLD.endpoint_generation, OLD.machine_generation, OLD.key_generation, OLD.wireguard_public_key, OLD.disco_public_key,
        OLD.quic_certificate_fingerprint, OLD.virtual_address, OLD.published_endpoints, OLD.revoked_at)
   IS DISTINCT FROM (NEW.role, NEW.machine_id, NEW.endpoint_generation, NEW.machine_generation, NEW.key_generation, NEW.wireguard_public_key, NEW.disco_public_key,
        NEW.quic_certificate_fingerprint, NEW.virtual_address, NEW.published_endpoints, NEW.revoked_at))
 EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();
