-- +goose Up
-- F9-04: a member granted access to a shared machine must be able to use it within seconds. A grant changes the
-- team policy version and a private access setting changes the team row; both published only to the team's hosts
-- (migrations 196, 206), so a member's daemon learned of the machine at its next inventory poll and
-- `pb access machine` said "No machine has that name or ID" for over two minutes. Both now also move the members'
-- account inventory topic, which every daemon of those accounts waits on (machineevents account topics).
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_team_members_inventory(wanted_team text) RETURNS void
    LANGUAGE sql AS $$
 SELECT paperboat.publish_account_topic(coalesce((SELECT array_agg(m.account_id) FROM paperboat.team_members m
  WHERE m.team_id = wanted_team AND m.active), '{}'::text[]), 'inventory');
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_policy_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.team_id IS NOT NULL THEN
    PERFORM paperboat.publish_team_hosts_peer_network(NEW.team_id);
    PERFORM paperboat.publish_team_members_inventory(NEW.team_id);
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_private_access_setting() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_team_hosts_peer_network(NEW.team_id);
  PERFORM paperboat.publish_team_members_inventory(NEW.team_id);
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_policy_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.team_id IS NOT NULL THEN
    PERFORM paperboat.publish_team_hosts_peer_network(NEW.team_id);
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_private_access_setting() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_team_hosts_peer_network(NEW.team_id);
  RETURN NULL;
END $$;
-- +goose StatementEnd
DROP FUNCTION paperboat.publish_team_members_inventory(text);
