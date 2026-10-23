-- +goose Up
-- The revocation trigger bumped the owner's peer_network topic once per inserted row: revoking 9,000 certificates in
-- one statement updated the same account_topic_generations row 9,000 times, each update walking the versions the
-- statement had already left (2.7 s of trigger time, quadratic). It now runs once per statement over its transition
-- table: one bump per account however many rows (6.6 ms). Prior art: PostgreSQL transition tables
-- (REFERENCING NEW TABLE) are the documented way to make a per-row trigger set-based.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_certificate_revocations() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT user_id FROM new_revocations), 'peer_network');
  RETURN NULL;
END $$;
-- +goose StatementEnd
DROP TRIGGER peer_endpoint_certificate_revocations_publish ON paperboat.peer_endpoint_certificate_revocations;
CREATE TRIGGER peer_endpoint_certificate_revocations_publish
    AFTER INSERT ON paperboat.peer_endpoint_certificate_revocations
    REFERENCING NEW TABLE AS new_revocations
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_peer_certificate_revocations();

-- +goose Down
DROP TRIGGER peer_endpoint_certificate_revocations_publish ON paperboat.peer_endpoint_certificate_revocations;
CREATE TRIGGER peer_endpoint_certificate_revocations_publish
    AFTER INSERT ON paperboat.peer_endpoint_certificate_revocations
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_certificate_revocation();
DROP FUNCTION paperboat.publish_peer_certificate_revocations();
