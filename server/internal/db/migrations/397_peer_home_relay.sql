-- +goose Up
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
-- A machine reports the regional relay it is homed on (its node id). Peers are given it on the machine's peer
-- entry and send their relay handshake there, as Tailscale peers learn a node's HomeDERP from the control plane
-- (tailcfg Node.HomeDERP in the netmap): a machine that restarted on the other relay of the pair was unreachable
-- while peers kept its old relay (hop-89). Its change publishes peer_network to the identity's audience at once,
-- like carrier_offer (223); the column is rewritten only when the value changes, so the topic does not flap.
-- No CHECK: only a node id of an eligible relay of the account is ever written (endpointsUpdate), and the request
-- is bounded in the service before that.
ALTER TABLE paperboat.peer_network_identities ADD COLUMN home_relay text;

CREATE TRIGGER peer_network_identities_publish_home_relay
    AFTER UPDATE OF home_relay ON paperboat.peer_network_identities
    FOR EACH ROW WHEN (OLD.home_relay IS DISTINCT FROM NEW.home_relay)
    EXECUTE FUNCTION paperboat.publish_peer_identity();

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
DROP TRIGGER peer_network_identities_publish_home_relay ON paperboat.peer_network_identities;
ALTER TABLE paperboat.peer_network_identities DROP COLUMN home_relay;
