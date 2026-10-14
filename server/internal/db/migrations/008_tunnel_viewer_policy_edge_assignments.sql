-- +goose Up
CREATE VIEW paperboat.current_tunnel_route_viewer_policy AS
  SELECT
    r.id AS route_id,
    CASE
      WHEN t.access_mode = 'public' AND r.protocol = 'http' AND p.audience IS NOT NULL THEN
        CASE p.audience
          WHEN 'public' THEN 'public'
          WHEN 'team' THEN 'team'
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

ALTER TABLE paperboat.tunnel_edge_route_assignments
  ADD COLUMN viewer_policy_generation bigint;

UPDATE paperboat.tunnel_edge_route_assignments a
SET access_mode = policy.effective_access_mode,
    viewer_policy_generation = policy.viewer_policy_generation
FROM paperboat.current_tunnel_route_viewer_policy policy
WHERE policy.route_id = a.route_id;

ALTER TABLE paperboat.tunnel_edge_route_assignments
  ALTER COLUMN viewer_policy_generation SET NOT NULL,
  ADD CONSTRAINT tunnel_edge_route_assignments_viewer_policy_generation_check
    CHECK (viewer_policy_generation > 0);

-- +goose Down
ALTER TABLE paperboat.tunnel_edge_route_assignments
  DROP CONSTRAINT tunnel_edge_route_assignments_viewer_policy_generation_check,
  DROP COLUMN viewer_policy_generation;
DROP VIEW paperboat.current_tunnel_route_viewer_policy;
