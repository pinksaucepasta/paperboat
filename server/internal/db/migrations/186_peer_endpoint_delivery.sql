-- +goose Up
-- Direct paths without a relay (PLAN 3b, N5, step 2): an identity's reported endpoints are delivered on its peer
-- entry. endpoints (migration 181) stays the latest report; published_endpoints is the set peers are given,
-- replaced at most once per minute per identity (a flapping NAT moves no peer more often; the latest set wins),
-- and its change alone publishes peer_network to the identity's audience. A set held back inside the minute is
-- due at endpoints_due_at, when the server publishes it itself (peers renew because of the publication, so it
-- cannot wait for the identity's next request). endpoints_capable marks a recipient
-- whose engine has sent an endpoints field (even an empty one): only such recipients are given peers' endpoints,
-- because older engines refuse an unknown field in the signed configuration. Never unset.
ALTER TABLE paperboat.peer_network_identities
    ADD COLUMN endpoints_capable boolean NOT NULL DEFAULT false,
    ADD COLUMN published_endpoints text[] CHECK (published_endpoints IS NULL OR cardinality(published_endpoints) <= 8),
    ADD COLUMN endpoints_published_at timestamptz,
    ADD COLUMN endpoints_due_at timestamptz;

-- small-table: one row per endpoint; the index holds only identities with a held-back set (none in steady state).
CREATE INDEX peer_network_identities_endpoints_due ON paperboat.peer_network_identities (endpoints_due_at)
    WHERE endpoints_due_at IS NOT NULL;

CREATE TRIGGER peer_network_identities_publish_endpoints
    AFTER UPDATE OF published_endpoints ON paperboat.peer_network_identities
    FOR EACH ROW WHEN (OLD.published_endpoints IS DISTINCT FROM NEW.published_endpoints)
    EXECUTE FUNCTION paperboat.publish_peer_identity();

-- +goose Down
DROP TRIGGER peer_network_identities_publish_endpoints ON paperboat.peer_network_identities;
DROP INDEX paperboat.peer_network_identities_endpoints_due;
ALTER TABLE paperboat.peer_network_identities
    DROP COLUMN endpoints_due_at,
    DROP COLUMN endpoints_published_at,
    DROP COLUMN published_endpoints,
    DROP COLUMN endpoints_capable;
