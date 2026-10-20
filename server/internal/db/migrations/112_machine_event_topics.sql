-- +goose Up
-- The remaining machine event topics (inventory, peer_network, tunnels, config) and a wider audience
-- for previews, published by the writes that change what a host caches. See 110 for the mechanism.

-- machine_audience: the machines that can see the machine: every machine of its owner, of the
-- members of its owning team and of the members of a team it is granted to.
-- +goose StatementBegin
CREATE FUNCTION paperboat.machine_audience(wanted_machine text) RETURNS text[]
    LANGUAGE sql STABLE AS $$
 SELECT coalesce(array_agg(DISTINCT audience.id), '{}'::text[]) FROM (
  SELECT peer.id FROM paperboat.user_machines own
   JOIN paperboat.user_machines peer ON peer.user_id = own.user_id AND peer.deleted_at IS NULL
   WHERE own.id = wanted_machine
  UNION
  SELECT peer.id FROM paperboat.user_machines own
   JOIN paperboat.team_members member ON member.team_id = own.owner_team_id AND member.active
   JOIN paperboat.user_machines peer ON peer.user_id = member.account_id AND peer.deleted_at IS NULL
   WHERE own.id = wanted_machine AND own.owner_team_id IS NOT NULL
  UNION
  SELECT peer.id FROM paperboat.team_machine_grants grant_row
   JOIN paperboat.team_members member ON member.team_id = grant_row.team_id AND member.active
    AND (grant_row.audience = 'all_members' OR member.account_id = grant_row.account_id)
   JOIN paperboat.user_machines peer ON peer.user_id = member.account_id AND peer.deleted_at IS NULL
   WHERE grant_row.machine_id = wanted_machine AND grant_row.active
 ) audience
$$;
-- +goose StatementEnd

-- account_machine_ids: every machine of one account.
-- +goose StatementBegin
CREATE FUNCTION paperboat.account_machine_ids(wanted_user text) RETURNS text[]
    LANGUAGE sql STABLE AS $$
 SELECT coalesce(array_agg(id), '{}'::text[]) FROM paperboat.user_machines WHERE user_id = wanted_user AND deleted_at IS NULL
$$;
-- +goose StatementEnd

-- A private preview is visible to everything that can see its owner machine, not only that machine.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_preview() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.machine_audience(NEW.owner_machine_id), 'previews');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- inventory: the machine list and each machine's services, presence and name.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_machine_inventory() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.machine_audience(NEW.id), 'inventory');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER user_machines_publish_inventory_insert
    AFTER INSERT ON paperboat.user_machines
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_machine_inventory();
CREATE TRIGGER user_machines_publish_inventory_update
    AFTER UPDATE OF alias, state, online, seat_state, deleted_at, revoked_at, owner_team_id ON paperboat.user_machines
    FOR EACH ROW WHEN (OLD.alias IS DISTINCT FROM NEW.alias OR OLD.state IS DISTINCT FROM NEW.state OR OLD.online IS DISTINCT FROM NEW.online
        OR OLD.seat_state IS DISTINCT FROM NEW.seat_state OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
        OR OLD.revoked_at IS DISTINCT FROM NEW.revoked_at OR OLD.owner_team_id IS DISTINCT FROM NEW.owner_team_id)
    EXECUTE FUNCTION paperboat.publish_machine_inventory();

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_machine_services() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.machine_audience(NEW.machine_id), 'inventory');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER machine_service_snapshots_publish_insert
    AFTER INSERT ON paperboat.machine_service_snapshots
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_machine_services();
CREATE TRIGGER machine_service_snapshots_publish_update
    AFTER UPDATE OF services ON paperboat.machine_service_snapshots
    FOR EACH ROW WHEN (OLD.services IS DISTINCT FROM NEW.services)
    EXECUTE FUNCTION paperboat.publish_machine_services();

-- Who can see a machine changes with a team grant or with team membership.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_access_grant() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY[NEW.machine_id], 'machine_access');
  PERFORM paperboat.publish_machine_topic(paperboat.machine_audience(NEW.machine_id), 'inventory');
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_team_membership() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.account_machine_ids(NEW.account_id), 'inventory');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER team_members_publish_insert
    AFTER INSERT ON paperboat.team_members
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_team_membership();
CREATE TRIGGER team_members_publish_update
    AFTER UPDATE OF active, role ON paperboat.team_members
    FOR EACH ROW WHEN (OLD.active IS DISTINCT FROM NEW.active OR OLD.role IS DISTINCT FROM NEW.role)
    EXECUTE FUNCTION paperboat.publish_team_membership();

-- peer_network: the keys, addresses and certificates the peers of a machine use.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_identity() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.machine_id IS NOT NULL THEN
    PERFORM paperboat.publish_machine_topic(paperboat.machine_audience(NEW.machine_id), 'peer_network');
  ELSE
    PERFORM paperboat.publish_machine_topic(paperboat.account_machine_ids(NEW.user_id), 'peer_network');
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER peer_network_identities_publish_insert
    AFTER INSERT ON paperboat.peer_network_identities
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_identity();
CREATE TRIGGER peer_network_identities_publish_update
    AFTER UPDATE OF wireguard_public_key, quic_certificate_fingerprint, virtual_address, key_generation, revoked_at, disco_public_key ON paperboat.peer_network_identities
    FOR EACH ROW WHEN (OLD.wireguard_public_key IS DISTINCT FROM NEW.wireguard_public_key
        OR OLD.quic_certificate_fingerprint IS DISTINCT FROM NEW.quic_certificate_fingerprint
        OR OLD.virtual_address IS DISTINCT FROM NEW.virtual_address OR OLD.key_generation IS DISTINCT FROM NEW.key_generation
        OR OLD.revoked_at IS DISTINCT FROM NEW.revoked_at OR OLD.disco_public_key IS DISTINCT FROM NEW.disco_public_key)
    EXECUTE FUNCTION paperboat.publish_peer_identity();

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_certificate_revocation() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.account_machine_ids(NEW.user_id), 'peer_network');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER peer_endpoint_certificate_revocations_publish
    AFTER INSERT ON paperboat.peer_endpoint_certificate_revocations
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_peer_certificate_revocation();

-- tunnels: a tunnel's configuration, state and connectors, for the machines that host its connectors.
-- +goose StatementBegin
CREATE FUNCTION paperboat.tunnel_host_ids(wanted_tunnel text) RETURNS text[]
    LANGUAGE sql STABLE AS $$
 SELECT coalesce(array_agg(DISTINCT host_id), '{}'::text[]) FROM (
  SELECT host_id FROM paperboat.tunnel_connectors WHERE tunnel_id = wanted_tunnel AND revoked_at IS NULL
  UNION SELECT created_by_host_id FROM paperboat.tunnels WHERE id = wanted_tunnel
 ) hosts
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_tunnel_config() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.tunnel_host_ids(NEW.tunnel_id), 'tunnels');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER tunnel_config_generations_publish_insert
    AFTER INSERT ON paperboat.tunnel_config_generations
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_tunnel_config();
CREATE TRIGGER tunnel_config_generations_publish_update
    AFTER UPDATE OF activation_state ON paperboat.tunnel_config_generations
    FOR EACH ROW WHEN (OLD.activation_state IS DISTINCT FROM NEW.activation_state)
    EXECUTE FUNCTION paperboat.publish_tunnel_config();

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_tunnel_state() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(paperboat.tunnel_host_ids(NEW.id), 'tunnels');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER tunnels_publish_update
    AFTER UPDATE OF desired_state, access_mode, expires_at, deleted_at ON paperboat.tunnels
    FOR EACH ROW WHEN (OLD.desired_state IS DISTINCT FROM NEW.desired_state OR OLD.access_mode IS DISTINCT FROM NEW.access_mode
        OR OLD.expires_at IS DISTINCT FROM NEW.expires_at OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at)
    EXECUTE FUNCTION paperboat.publish_tunnel_state();

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_tunnel_connector() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY[NEW.host_id], 'tunnels');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER tunnel_connectors_publish_insert
    AFTER INSERT ON paperboat.tunnel_connectors
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_tunnel_connector();
CREATE TRIGGER tunnel_connectors_publish_update
    AFTER UPDATE OF desired_state, drain_state, revoked_at, rotation_generation ON paperboat.tunnel_connectors
    FOR EACH ROW WHEN (OLD.desired_state IS DISTINCT FROM NEW.desired_state OR OLD.drain_state IS DISTINCT FROM NEW.drain_state
        OR OLD.revoked_at IS DISTINCT FROM NEW.revoked_at OR OLD.rotation_generation IS DISTINCT FROM NEW.rotation_generation)
    EXECUTE FUNCTION paperboat.publish_tunnel_connector();

-- config: a machine's configuration sync assignment, and the team default it adopted.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_config_assignment() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY[NEW.machine_id], 'config');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER control_config_assignments_publish_insert
    AFTER INSERT ON paperboat.control_config_assignments
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_config_assignment();
CREATE TRIGGER control_config_assignments_publish_update
    AFTER UPDATE OF version ON paperboat.control_config_assignments
    FOR EACH ROW WHEN (OLD.version IS DISTINCT FROM NEW.version)
    EXECUTE FUNCTION paperboat.publish_config_assignment();

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_team_config_default() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(
    (SELECT coalesce(array_agg(machine_id), '{}'::text[]) FROM paperboat.control_config_assignments WHERE adopted_team_id = NEW.team_id), 'config');
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER team_config_defaults_publish
    AFTER INSERT OR UPDATE OF version ON paperboat.team_config_defaults
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_team_config_default();

-- +goose Down
DROP TRIGGER team_config_defaults_publish ON paperboat.team_config_defaults;
DROP FUNCTION paperboat.publish_team_config_default();
DROP TRIGGER control_config_assignments_publish_update ON paperboat.control_config_assignments;
DROP TRIGGER control_config_assignments_publish_insert ON paperboat.control_config_assignments;
DROP FUNCTION paperboat.publish_config_assignment();
DROP TRIGGER tunnel_connectors_publish_update ON paperboat.tunnel_connectors;
DROP TRIGGER tunnel_connectors_publish_insert ON paperboat.tunnel_connectors;
DROP FUNCTION paperboat.publish_tunnel_connector();
DROP TRIGGER tunnels_publish_update ON paperboat.tunnels;
DROP FUNCTION paperboat.publish_tunnel_state();
DROP TRIGGER tunnel_config_generations_publish_update ON paperboat.tunnel_config_generations;
DROP TRIGGER tunnel_config_generations_publish_insert ON paperboat.tunnel_config_generations;
DROP FUNCTION paperboat.publish_tunnel_config();
DROP FUNCTION paperboat.tunnel_host_ids(text);
DROP TRIGGER peer_endpoint_certificate_revocations_publish ON paperboat.peer_endpoint_certificate_revocations;
DROP FUNCTION paperboat.publish_peer_certificate_revocation();
DROP TRIGGER peer_network_identities_publish_update ON paperboat.peer_network_identities;
DROP TRIGGER peer_network_identities_publish_insert ON paperboat.peer_network_identities;
DROP FUNCTION paperboat.publish_peer_identity();
DROP TRIGGER team_members_publish_update ON paperboat.team_members;
DROP TRIGGER team_members_publish_insert ON paperboat.team_members;
DROP FUNCTION paperboat.publish_team_membership();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_access_grant() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY[NEW.machine_id], 'machine_access');
  RETURN NEW;
END $$;
-- +goose StatementEnd
DROP TRIGGER machine_service_snapshots_publish_update ON paperboat.machine_service_snapshots;
DROP TRIGGER machine_service_snapshots_publish_insert ON paperboat.machine_service_snapshots;
DROP FUNCTION paperboat.publish_machine_services();
DROP TRIGGER user_machines_publish_inventory_update ON paperboat.user_machines;
DROP TRIGGER user_machines_publish_inventory_insert ON paperboat.user_machines;
DROP FUNCTION paperboat.publish_machine_inventory();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_machine_preview() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY[NEW.owner_machine_id], 'previews');
  RETURN NEW;
END $$;
-- +goose StatementEnd
DROP FUNCTION paperboat.account_machine_ids(text);
DROP FUNCTION paperboat.machine_audience(text);
