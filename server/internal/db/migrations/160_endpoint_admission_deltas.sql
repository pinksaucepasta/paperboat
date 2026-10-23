-- +goose Up
-- Endpoint admission deltas (Lane W scale matrix, PLAN 3a): an edge pulls only the endpoint
-- assignments that changed since its cursor instead of its whole set every poll. changed_at moves
-- whenever what EdgeAdmissions would answer for the row can change; time-only changes (a lease
-- lapsing, a carrier going stale) and cascade deletes are caught by the edge's periodic full pull.
-- Prior art: Tailscale sends a full map once, then PeersChanged/PeersRemoved deltas (tailcfg).

ALTER TABLE paperboat.endpoint_edge_assignments ADD COLUMN changed_at timestamptz NOT NULL DEFAULT clock_timestamp();
CREATE INDEX endpoint_edge_assignments_changed ON paperboat.endpoint_edge_assignments(edge_node_id, edge_process_epoch, changed_at);

-- +goose StatementBegin
CREATE FUNCTION paperboat.endpoint_assignment_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' OR (OLD.desired, OLD.generation, OLD.edge_process_epoch, OLD.carrier_session_id)
     IS DISTINCT FROM (NEW.desired, NEW.generation, NEW.edge_process_epoch, NEW.carrier_session_id) THEN
    NEW.changed_at := clock_timestamp();
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER endpoint_edge_assignments_changed BEFORE INSERT OR UPDATE ON paperboat.endpoint_edge_assignments
    FOR EACH ROW EXECUTE FUNCTION paperboat.endpoint_assignment_changed();

-- +goose StatementBegin
CREATE FUNCTION paperboat.endpoint_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE paperboat.endpoint_edge_assignments SET changed_at = clock_timestamp() WHERE endpoint_id = NEW.id;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER endpoints_changed AFTER UPDATE OF desired_state, access_mode ON paperboat.endpoints
    FOR EACH ROW WHEN ((OLD.desired_state, OLD.access_mode) IS DISTINCT FROM (NEW.desired_state, NEW.access_mode))
    EXECUTE FUNCTION paperboat.endpoint_changed();

-- +goose StatementBegin
CREATE FUNCTION paperboat.endpoint_route_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE paperboat.endpoint_edge_assignments SET changed_at = clock_timestamp() WHERE route_id = NEW.id;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER endpoint_routes_changed AFTER UPDATE OF hostname, revision ON paperboat.endpoint_routes
    FOR EACH ROW WHEN ((OLD.hostname, OLD.revision) IS DISTINCT FROM (NEW.hostname, NEW.revision))
    EXECUTE FUNCTION paperboat.endpoint_route_changed();

-- +goose StatementBegin
CREATE FUNCTION paperboat.endpoint_lease_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE paperboat.endpoint_edge_assignments x SET changed_at = clock_timestamp()
  FROM paperboat.endpoints e WHERE e.preview_lease_id = NEW.id AND x.endpoint_id = e.id;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER preview_leases_endpoint_changed AFTER UPDATE OF terminal_state ON paperboat.preview_leases
    FOR EACH ROW WHEN (OLD.terminal_state IS DISTINCT FROM NEW.terminal_state)
    EXECUTE FUNCTION paperboat.endpoint_lease_changed();

-- +goose StatementBegin
CREATE FUNCTION paperboat.endpoint_lease_attachment_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE paperboat.endpoint_edge_assignments x SET changed_at = clock_timestamp()
  FROM paperboat.endpoints e WHERE e.preview_lease_id = NEW.preview_id AND x.endpoint_id = e.id;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER preview_lease_attachments_endpoint_insert AFTER INSERT ON paperboat.preview_lease_carrier_attachments
    FOR EACH ROW EXECUTE FUNCTION paperboat.endpoint_lease_attachment_changed();
CREATE TRIGGER preview_lease_attachments_endpoint_update AFTER UPDATE OF state ON paperboat.preview_lease_carrier_attachments
    FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION paperboat.endpoint_lease_attachment_changed();

-- +goose StatementBegin
CREATE FUNCTION paperboat.endpoint_carrier_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE paperboat.endpoint_edge_assignments SET changed_at = clock_timestamp()
  WHERE edge_node_id = NEW.edge_node_id AND carrier_session_id = NEW.session_id;
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- A renewal rewrites only expires_at inside the admission; that must not count as a change.
CREATE TRIGGER runtime_carrier_attachments_endpoint_changed AFTER UPDATE OF admission ON paperboat.runtime_carrier_attachments
    FOR EACH ROW WHEN ((OLD.admission - 'expires_at') IS DISTINCT FROM (NEW.admission - 'expires_at'))
    EXECUTE FUNCTION paperboat.endpoint_carrier_changed();

-- +goose Down
DROP TRIGGER runtime_carrier_attachments_endpoint_changed ON paperboat.runtime_carrier_attachments;
DROP FUNCTION paperboat.endpoint_carrier_changed();
DROP TRIGGER preview_lease_attachments_endpoint_update ON paperboat.preview_lease_carrier_attachments;
DROP TRIGGER preview_lease_attachments_endpoint_insert ON paperboat.preview_lease_carrier_attachments;
DROP FUNCTION paperboat.endpoint_lease_attachment_changed();
DROP TRIGGER preview_leases_endpoint_changed ON paperboat.preview_leases;
DROP FUNCTION paperboat.endpoint_lease_changed();
DROP TRIGGER endpoint_routes_changed ON paperboat.endpoint_routes;
DROP FUNCTION paperboat.endpoint_route_changed();
DROP TRIGGER endpoints_changed ON paperboat.endpoints;
DROP FUNCTION paperboat.endpoint_changed();
DROP TRIGGER endpoint_edge_assignments_changed ON paperboat.endpoint_edge_assignments;
DROP FUNCTION paperboat.endpoint_assignment_changed();
DROP INDEX paperboat.endpoint_edge_assignments_changed;
ALTER TABLE paperboat.endpoint_edge_assignments DROP COLUMN changed_at;
