-- +goose Up
-- Every write that changes what the machine private-routes answer can change moves the account's
-- private_routes topic once (the accessors are every machine of the route's account). Time-only changes
-- (a lease or carrier lapsing, observed_at and heartbeat refreshes, a carrier renewal that rewrites only
-- expires_at) publish nothing; each route's own expires_at covers those on the client.
-- Prior art: headscale hscontrol/poll.go (one long poll per node, woken by state changes).
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_private_routes_endpoint() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY(SELECT account_id FROM paperboat.endpoints WHERE id = NEW.endpoint_id), 'private_routes');
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- Statement triggers (transition tables), as 168 and 193: a bulk change moves the topic once per account.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_private_routes_assignments() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT e.account_id FROM new_rows n JOIN paperboat.endpoints e ON e.id = n.endpoint_id), 'private_routes');
  ELSE
    PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT e.account_id FROM new_rows n
      JOIN old_rows o ON o.route_id = n.route_id AND o.edge_node_id = n.edge_node_id
      JOIN paperboat.endpoints e ON e.id = n.endpoint_id
      WHERE (o.desired, o.observed_ready, o.generation, o.carrier_session_id, o.edge_process_epoch)
            IS DISTINCT FROM (n.desired, n.observed_ready, n.generation, n.carrier_session_id, n.edge_process_epoch)), 'private_routes');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_private_routes_endpoints() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT n.account_id FROM new_rows n JOIN old_rows o ON o.id = n.id
    WHERE (o.desired_state, o.access_mode) IS DISTINCT FROM (n.desired_state, n.access_mode)), 'private_routes');
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_private_routes_leases() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT n.account_id FROM new_rows n JOIN old_rows o ON o.id = n.id
    WHERE (o.terminal_state, o.access_mode) IS DISTINCT FROM (n.terminal_state, n.access_mode)), 'private_routes');
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_private_routes_carriers() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT account_id FROM new_rows), 'private_routes');
  ELSE
    PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT n.account_id FROM new_rows n JOIN old_rows o ON o.route_id = n.route_id
      WHERE (o.session_id, o.edge_process_epoch, o.admission - 'expires_at') IS DISTINCT FROM (n.session_id, n.edge_process_epoch, n.admission - 'expires_at')), 'private_routes');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_private_routes_account() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY[NEW.account_id], 'private_routes');
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_private_routes_tunnel() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY(SELECT account_id FROM paperboat.tunnels WHERE id = NEW.tunnel_id), 'private_routes');
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_private_routes_viewer_policy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_account_topic(ARRAY[NEW.owner_account_id], 'private_routes');
  RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER endpoint_edge_assignments_private_routes_insert AFTER INSERT ON paperboat.endpoint_edge_assignments
    REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_private_routes_assignments();
CREATE TRIGGER endpoint_edge_assignments_private_routes_update AFTER UPDATE ON paperboat.endpoint_edge_assignments
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_private_routes_assignments();

CREATE TRIGGER endpoints_private_routes AFTER UPDATE ON paperboat.endpoints
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_private_routes_endpoints();

CREATE TRIGGER endpoint_routes_private_routes AFTER UPDATE OF hostname, revision ON paperboat.endpoint_routes
    FOR EACH ROW WHEN ((OLD.hostname, OLD.revision) IS DISTINCT FROM (NEW.hostname, NEW.revision))
    EXECUTE FUNCTION paperboat.publish_private_routes_endpoint();

CREATE TRIGGER preview_leases_private_routes AFTER UPDATE ON paperboat.preview_leases
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_private_routes_leases();

CREATE TRIGGER runtime_carrier_attachments_private_routes_insert AFTER INSERT ON paperboat.runtime_carrier_attachments
    REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_private_routes_carriers();
CREATE TRIGGER runtime_carrier_attachments_private_routes_update AFTER UPDATE ON paperboat.runtime_carrier_attachments
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_private_routes_carriers();

CREATE TRIGGER tunnels_private_routes AFTER UPDATE OF desired_state, access_mode, expires_at, deleted_at ON paperboat.tunnels
    FOR EACH ROW WHEN ((OLD.desired_state, OLD.access_mode, OLD.expires_at, OLD.deleted_at)
        IS DISTINCT FROM (NEW.desired_state, NEW.access_mode, NEW.expires_at, NEW.deleted_at))
    EXECUTE FUNCTION paperboat.publish_private_routes_account();

CREATE TRIGGER tunnel_routes_private_routes AFTER UPDATE OF desired_state, deleted_at, generation, match_hostname ON paperboat.tunnel_routes
    FOR EACH ROW WHEN ((OLD.desired_state, OLD.deleted_at, OLD.generation, OLD.match_hostname)
        IS DISTINCT FROM (NEW.desired_state, NEW.deleted_at, NEW.generation, NEW.match_hostname))
    EXECUTE FUNCTION paperboat.publish_private_routes_tunnel();

CREATE TRIGGER browser_viewer_policies_private_routes AFTER INSERT OR UPDATE OF audience, generation ON paperboat.browser_viewer_policies
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_private_routes_viewer_policy();

-- +goose Down
DROP TRIGGER browser_viewer_policies_private_routes ON paperboat.browser_viewer_policies;
DROP TRIGGER tunnel_routes_private_routes ON paperboat.tunnel_routes;
DROP TRIGGER tunnels_private_routes ON paperboat.tunnels;
DROP TRIGGER runtime_carrier_attachments_private_routes_update ON paperboat.runtime_carrier_attachments;
DROP TRIGGER runtime_carrier_attachments_private_routes_insert ON paperboat.runtime_carrier_attachments;
DROP TRIGGER preview_leases_private_routes ON paperboat.preview_leases;
DROP TRIGGER endpoint_routes_private_routes ON paperboat.endpoint_routes;
DROP TRIGGER endpoints_private_routes ON paperboat.endpoints;
DROP TRIGGER endpoint_edge_assignments_private_routes_update ON paperboat.endpoint_edge_assignments;
DROP TRIGGER endpoint_edge_assignments_private_routes_insert ON paperboat.endpoint_edge_assignments;
DROP FUNCTION paperboat.publish_private_routes_viewer_policy();
DROP FUNCTION paperboat.publish_private_routes_tunnel();
DROP FUNCTION paperboat.publish_private_routes_endpoint();
DROP FUNCTION paperboat.publish_private_routes_account();
DROP FUNCTION paperboat.publish_private_routes_carriers();
DROP FUNCTION paperboat.publish_private_routes_leases();
DROP FUNCTION paperboat.publish_private_routes_endpoints();
DROP FUNCTION paperboat.publish_private_routes_assignments();
