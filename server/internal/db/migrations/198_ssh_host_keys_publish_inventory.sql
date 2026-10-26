-- +goose Up
-- The inventory carries each machine's active SSH host keys (MachineSSH.HostKeys), so a promotion (pending -> active,
-- the old active set -> superseded) changes what daemons serve as known_hosts: it publishes the inventory topic to the
-- machine's audience, as every other inventory change does.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_ssh_host_key_set() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(paperboat.machine_audience_accounts(NEW.user_machine_id), 'inventory');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER machine_ssh_host_key_sets_publish_insert AFTER INSERT ON paperboat.machine_ssh_host_key_sets
    FOR EACH ROW WHEN (NEW.state = 'active') EXECUTE FUNCTION paperboat.publish_ssh_host_key_set();
CREATE TRIGGER machine_ssh_host_key_sets_publish_update AFTER UPDATE OF state ON paperboat.machine_ssh_host_key_sets
    FOR EACH ROW WHEN (NEW.state = 'active' AND OLD.state IS DISTINCT FROM NEW.state) EXECUTE FUNCTION paperboat.publish_ssh_host_key_set();

-- +goose Down
DROP TRIGGER machine_ssh_host_key_sets_publish_update ON paperboat.machine_ssh_host_key_sets;
DROP TRIGGER machine_ssh_host_key_sets_publish_insert ON paperboat.machine_ssh_host_key_sets;
DROP FUNCTION paperboat.publish_ssh_host_key_set();
