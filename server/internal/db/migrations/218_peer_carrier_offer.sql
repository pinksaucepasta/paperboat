-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Fresh connects to a moved machine: the machine reports its active carrier generation's machine-signed offer
-- (native.CarrierOffer bytes, at most 8 KiB, bound-checked by the server before the write). Each report replaces
-- the last; peers are given it unchanged on the machine's peer entry until carrier_offer_expires_at. Its change
-- publishes peer_network to the identity's audience at once, without published_endpoints' once-a-minute hold:
-- peers must learn the generation before the old carrier restarts.
ALTER TABLE paperboat.peer_network_identities
    ADD COLUMN carrier_offer bytea,
    ADD COLUMN carrier_offer_expires_at timestamptz;

CREATE TRIGGER peer_network_identities_publish_carrier_offer
    AFTER UPDATE OF carrier_offer ON paperboat.peer_network_identities
    FOR EACH ROW WHEN (OLD.carrier_offer IS DISTINCT FROM NEW.carrier_offer)
    EXECUTE FUNCTION paperboat.publish_peer_identity();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_network_identities_publish_carrier_offer ON paperboat.peer_network_identities;
ALTER TABLE paperboat.peer_network_identities
    DROP COLUMN carrier_offer_expires_at,
    DROP COLUMN carrier_offer;
