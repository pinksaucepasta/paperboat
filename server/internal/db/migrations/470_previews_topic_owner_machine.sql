-- +goose Up
-- A preview lease change was published to every account the owner machine's audience reaches (migration 150), so each
-- machine of the account woke its held wait, ran the topic re-read and refetched, though nothing but the owner machine
-- reads the previews topic of a lease: the host runtime's carrier attachment (own routes) and the lease observer, which
-- waits on the owner machine's events. Production 11-20: one 84-machine account bumped previews about every 5 s and woke
-- all of them. The topic now moves for the owner machine only; machine_services, inventory, peer_network and
-- private_routes stay audience-wide because every machine of the audience reads them.
-- Prior art: headscale hscontrol/mapper/batcher.go (a change is queued for the nodes it concerns, not for every node).
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_preview() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY[NEW.owner_machine_id], 'previews');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_preview() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(paperboat.machine_audience_accounts(NEW.owner_machine_id), 'previews');
  RETURN NEW;
END $$;
-- +goose StatementEnd
