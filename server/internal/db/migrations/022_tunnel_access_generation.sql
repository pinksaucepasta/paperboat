-- +goose Up
ALTER TABLE paperboat.tunnels ADD COLUMN access_generation bigint NOT NULL DEFAULT 1 CHECK (access_generation > 0);
UPDATE paperboat.tunnels SET access_generation=generation;
-- Resource edits and access revocations have separate authority revisions.
-- +goose StatementBegin
CREATE FUNCTION paperboat.advance_tunnel_access_generation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.access_mode IS DISTINCT FROM OLD.access_mode THEN
    NEW.access_generation := GREATEST(OLD.access_generation + 1, NEW.generation);
  ELSE
    NEW.access_generation := OLD.access_generation;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER tunnel_access_generation BEFORE UPDATE ON paperboat.tunnels FOR EACH ROW EXECUTE FUNCTION paperboat.advance_tunnel_access_generation();
CREATE OR REPLACE VIEW paperboat.current_tunnel_route_viewer_policy AS
  SELECT
    r.id AS route_id,
    CASE
      WHEN r.protocol = 'http' AND p.audience IS NOT NULL THEN
        CASE
          WHEN t.access_mode = 'public' AND p.audience = 'public' THEN 'public'
          ELSE 'private'
        END
      ELSE t.access_mode
    END::text AS effective_access_mode,
    COALESCE(p.generation, t.access_generation)::bigint AS viewer_policy_generation
  FROM paperboat.tunnel_routes r
  JOIN paperboat.tunnels t ON t.id = r.tunnel_id
  LEFT JOIN paperboat.browser_viewer_policies p
    ON p.resource_kind = 'tunnel'
   AND p.resource_id = t.id
   AND p.route_id = r.id
   AND p.owner_account_id = t.account_id;


-- +goose Down
CREATE OR REPLACE VIEW paperboat.current_tunnel_route_viewer_policy AS
  SELECT
    r.id AS route_id,
    CASE
      WHEN r.protocol = 'http' AND p.audience IS NOT NULL THEN
        CASE
          WHEN t.access_mode = 'public' AND p.audience = 'public' THEN 'public'
          ELSE 'private'
        END
      ELSE t.access_mode
    END::text AS effective_access_mode,
    COALESCE(p.generation, t.generation)::bigint AS viewer_policy_generation
  FROM paperboat.tunnel_routes r
  JOIN paperboat.tunnels t ON t.id = r.tunnel_id
  LEFT JOIN paperboat.browser_viewer_policies p
    ON p.resource_kind = 'tunnel'
   AND p.resource_id = t.id
   AND p.route_id = r.id
   AND p.owner_account_id = t.account_id;

DROP TRIGGER tunnel_access_generation ON paperboat.tunnels;
DROP FUNCTION paperboat.advance_tunnel_access_generation();
ALTER TABLE paperboat.tunnels DROP COLUMN access_generation;
