-- +goose Up
SET LOCAL lock_timeout = '5s';
-- An edge holds its endpoint pull open (PullEndpoints wait_ms) and is answered the moment one of its assignments is
-- written, instead of at its next 500 ms poll: two edges polling in independent phases served a new preview up to
-- 900 ms apart, and a preview is ready only once every edge holds it. The notification names the edge
-- ('edge:<node id>', the machineevents Hub key); it carries no data and PostgreSQL merges repeats inside a
-- transaction. An update notifies when changed_at moved (the BEFORE trigger of 160 moves it on every material
-- change); a delete notifies because the edge must drop the route.
-- Prior art: headscale hscontrol/poll.go serveLongPoll answers a held map request on the node's update channel.

-- +goose StatementBegin
CREATE FUNCTION paperboat.notify_edge_assignment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('machine_events', 'edge:' || CASE WHEN TG_OP = 'DELETE' THEN OLD.edge_node_id ELSE NEW.edge_node_id END);
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER endpoint_edge_assignments_notify_insert AFTER INSERT ON paperboat.endpoint_edge_assignments
    FOR EACH ROW EXECUTE FUNCTION paperboat.notify_edge_assignment();
CREATE TRIGGER endpoint_edge_assignments_notify_update AFTER UPDATE ON paperboat.endpoint_edge_assignments
    FOR EACH ROW WHEN (OLD.changed_at IS DISTINCT FROM NEW.changed_at) EXECUTE FUNCTION paperboat.notify_edge_assignment();
CREATE TRIGGER endpoint_edge_assignments_notify_delete AFTER DELETE ON paperboat.endpoint_edge_assignments
    FOR EACH ROW EXECUTE FUNCTION paperboat.notify_edge_assignment();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER endpoint_edge_assignments_notify_insert ON paperboat.endpoint_edge_assignments;
DROP TRIGGER endpoint_edge_assignments_notify_update ON paperboat.endpoint_edge_assignments;
DROP TRIGGER endpoint_edge_assignments_notify_delete ON paperboat.endpoint_edge_assignments;
DROP FUNCTION paperboat.notify_edge_assignment();
