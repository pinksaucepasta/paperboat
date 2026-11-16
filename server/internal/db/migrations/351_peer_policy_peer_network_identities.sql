-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TRIGGER peer_policy AFTER INSERT OR DELETE ON paperboat.peer_network_identities
 FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.peer_network_identities FOR EACH ROW
 WHEN ((OLD.role, OLD.machine_id, OLD.endpoint_generation, OLD.machine_generation, OLD.key_generation, OLD.wireguard_public_key, OLD.disco_public_key,
        OLD.quic_certificate_fingerprint, OLD.virtual_address, OLD.published_endpoints, OLD.revoked_at)
   IS DISTINCT FROM (NEW.role, NEW.machine_id, NEW.endpoint_generation, NEW.machine_generation, NEW.key_generation, NEW.wireguard_public_key, NEW.disco_public_key,
        NEW.quic_certificate_fingerprint, NEW.virtual_address, NEW.published_endpoints, NEW.revoked_at))
 EXECUTE FUNCTION paperboat.publish_peer_policy_account_row();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy_update ON paperboat.peer_network_identities;
DROP TRIGGER peer_policy ON paperboat.peer_network_identities;
