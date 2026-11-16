-- +goose Up
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
-- A machine entering or leaving its account's peer mesh (peersessions meshEntriesQuery: state online/offline, an
-- occupied seat, not revoked, deleted or team-owned) moved only inventory, so every engine woke its peer-network
-- renewal on inventory too, and every rename, capability or lease change anywhere in the account made all of its
-- endpoints refetch their signed configuration (FIX-PERF-SERVER, 2026-11-16: one 89-machine account, a refetch per
-- endpoint about every 36 s). Membership changes now move peer_network, the one topic engines renew on.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_machine_mesh_membership() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(paperboat.machine_audience_accounts(NEW.id), 'peer_network');
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER user_machines_publish_mesh_membership
AFTER UPDATE OF state, seat_state, deleted_at, revoked_at, owner_team_id ON paperboat.user_machines
FOR EACH ROW WHEN (
  (OLD.state IN ('online','offline')) IS DISTINCT FROM (NEW.state IN ('online','offline'))
  OR OLD.seat_state IS DISTINCT FROM NEW.seat_state
  OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
  OR OLD.revoked_at IS DISTINCT FROM NEW.revoked_at
  OR OLD.owner_team_id IS DISTINCT FROM NEW.owner_team_id)
EXECUTE FUNCTION paperboat.publish_machine_mesh_membership();

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
DROP TRIGGER user_machines_publish_mesh_membership ON paperboat.user_machines;
DROP FUNCTION paperboat.publish_machine_mesh_membership();
