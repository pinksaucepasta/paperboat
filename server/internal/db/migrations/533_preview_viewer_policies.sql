-- +goose Up
-- A viewer policy names a preview route or a tunnel route, but its foreign key allowed only tunnel_routes, so no
-- preview policy could be saved. The writer locks and checks the route of either kind in the same transaction; the
-- two triggers keep the key's cascade, so a policy (and the addresses it lists) never outlives its tunnel route or
-- preview.
SET LOCAL lock_timeout = '5s';

ALTER TABLE paperboat.browser_viewer_policies DROP CONSTRAINT browser_viewer_policies_route_id_fkey;

-- +goose StatementBegin
CREATE FUNCTION paperboat.delete_tunnel_route_viewer_policies() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  DELETE FROM paperboat.browser_viewer_policies WHERE route_id = OLD.id AND resource_kind = 'tunnel';
  RETURN NULL;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.delete_preview_viewer_policies() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  DELETE FROM paperboat.browser_viewer_policies WHERE resource_kind = 'preview' AND resource_id = OLD.id;
  RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER tunnel_routes_viewer_policies_delete AFTER DELETE ON paperboat.tunnel_routes
    FOR EACH ROW EXECUTE FUNCTION paperboat.delete_tunnel_route_viewer_policies();
CREATE TRIGGER preview_leases_viewer_policies_delete AFTER DELETE ON paperboat.preview_leases
    FOR EACH ROW EXECUTE FUNCTION paperboat.delete_preview_viewer_policies();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER preview_leases_viewer_policies_delete ON paperboat.preview_leases;
DROP TRIGGER tunnel_routes_viewer_policies_delete ON paperboat.tunnel_routes;
DROP FUNCTION paperboat.delete_preview_viewer_policies();
DROP FUNCTION paperboat.delete_tunnel_route_viewer_policies();
-- NOT VALID: preview rows saved meanwhile have no tunnel route.
ALTER TABLE paperboat.browser_viewer_policies ADD CONSTRAINT browser_viewer_policies_route_id_fkey
    FOREIGN KEY (route_id) REFERENCES paperboat.tunnel_routes(id) ON DELETE CASCADE NOT VALID;
