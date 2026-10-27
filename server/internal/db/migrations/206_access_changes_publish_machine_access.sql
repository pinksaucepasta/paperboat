-- +goose Up
-- F9-04: a host must admit a newly granted peer within seconds. Migration 196 moves only peer_network on a machine
-- grant, a team binding or a team change, but on a carrier install the engine that admits native streams wakes on
-- machine_access alone (a refresh never publishes it, migration 203), so a new grant waited for the next renewal; a
-- change of the team's private access setting published nothing at all. Both now also move machine_access.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_hosts_peer_network(wanted_team text) RETURNS void
    LANGUAGE plpgsql AS $$
DECLARE hosts text[];
BEGIN
  hosts := coalesce((SELECT array_agg(b.resource_id) FROM paperboat.team_resource_bindings b
   WHERE b.team_id = wanted_team AND b.resource_kind = 'machine' AND b.active), '{}'::text[]);
  PERFORM paperboat.publish_machine_topic(hosts, 'peer_network');
  PERFORM paperboat.publish_machine_topic(hosts, 'machine_access');
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_grant_host_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE grant_row paperboat.team_resource_grants;
BEGIN
  grant_row := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
  IF grant_row.resource_kind = 'machine' THEN
    PERFORM paperboat.publish_machine_topic(ARRAY[grant_row.resource_id], 'peer_network');
    PERFORM paperboat.publish_machine_topic(ARRAY[grant_row.resource_id], 'machine_access');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_binding_host_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE binding_row paperboat.team_resource_bindings;
BEGIN
  binding_row := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
  IF binding_row.resource_kind = 'machine' THEN
    PERFORM paperboat.publish_machine_topic(ARRAY[binding_row.resource_id], 'peer_network');
    PERFORM paperboat.publish_machine_topic(ARRAY[binding_row.resource_id], 'machine_access');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_team_private_access_setting() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_team_hosts_peer_network(NEW.team_id);
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER teams_publish_private_access_setting
    AFTER UPDATE OF allow_private_access ON paperboat.teams
    FOR EACH ROW WHEN (old.allow_private_access IS DISTINCT FROM new.allow_private_access)
    EXECUTE FUNCTION paperboat.publish_team_private_access_setting();

-- +goose Down
DROP TRIGGER teams_publish_private_access_setting ON paperboat.teams;
DROP FUNCTION paperboat.publish_team_private_access_setting();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_hosts_peer_network(wanted_team text) RETURNS void
    LANGUAGE sql AS $$
 SELECT paperboat.publish_machine_topic(coalesce((SELECT array_agg(b.resource_id) FROM paperboat.team_resource_bindings b
  WHERE b.team_id = wanted_team AND b.resource_kind = 'machine' AND b.active), '{}'::text[]), 'peer_network');
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_grant_host_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE grant_row paperboat.team_resource_grants;
BEGIN
  grant_row := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
  IF grant_row.resource_kind = 'machine' THEN
    PERFORM paperboat.publish_machine_topic(ARRAY[grant_row.resource_id], 'peer_network');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_binding_host_peer_network() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE binding_row paperboat.team_resource_bindings;
BEGIN
  binding_row := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
  IF binding_row.resource_kind = 'machine' THEN
    PERFORM paperboat.publish_machine_topic(ARRAY[binding_row.resource_id], 'peer_network');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
