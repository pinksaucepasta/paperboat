-- +goose Up
-- The one-identity update hands the machine's network to the new version once, so a machine never offers a
-- second carrier generation to its peers. Drops what migration 223 added.
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_network_identities_publish_carrier_offer ON paperboat.peer_network_identities;
ALTER TABLE paperboat.peer_network_identities
    DROP COLUMN carrier_offer_expires_at,
    DROP COLUMN carrier_offer;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.peer_network_identities
    ADD COLUMN carrier_offer bytea,
    ADD COLUMN carrier_offer_expires_at timestamptz;

CREATE TRIGGER peer_network_identities_publish_carrier_offer
    AFTER UPDATE OF carrier_offer ON paperboat.peer_network_identities
    FOR EACH ROW WHEN (OLD.carrier_offer IS DISTINCT FROM NEW.carrier_offer)
    EXECUTE FUNCTION paperboat.publish_peer_identity();
