-- +goose Up
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

ALTER TABLE paperboat.browser_access_transactions
  ADD COLUMN viewer_policy_team_id text REFERENCES paperboat.teams(team_id),
  ADD COLUMN viewer_policy_team_generation bigint,
  ADD COLUMN viewer_policy_membership_generation bigint,
  ADD COLUMN viewer_policy_binding_generation bigint,
  ADD CONSTRAINT browser_access_transactions_viewer_policy_team_authority_check
    CHECK (
      (viewer_policy_team_id IS NULL
       AND viewer_policy_team_generation IS NULL
       AND viewer_policy_membership_generation IS NULL
       AND viewer_policy_binding_generation IS NULL)
      OR
      (viewer_policy_team_id IS NOT NULL
       AND viewer_policy_team_generation IS NOT NULL AND viewer_policy_team_generation > 0
       AND viewer_policy_membership_generation IS NOT NULL AND viewer_policy_membership_generation > 0
       AND viewer_policy_binding_generation IS NOT NULL AND viewer_policy_binding_generation > 0)
    );

ALTER TABLE paperboat.browser_access_sessions
  ADD COLUMN viewer_policy_team_id text REFERENCES paperboat.teams(team_id),
  ADD COLUMN viewer_policy_team_generation bigint,
  ADD COLUMN viewer_policy_membership_generation bigint,
  ADD COLUMN viewer_policy_binding_generation bigint,
  ADD CONSTRAINT browser_access_sessions_viewer_policy_team_authority_check
    CHECK (
      (viewer_policy_team_id IS NULL
       AND viewer_policy_team_generation IS NULL
       AND viewer_policy_membership_generation IS NULL
       AND viewer_policy_binding_generation IS NULL)
      OR
      (viewer_policy_team_id IS NOT NULL
       AND viewer_policy_team_generation IS NOT NULL AND viewer_policy_team_generation > 0
       AND viewer_policy_membership_generation IS NOT NULL AND viewer_policy_membership_generation > 0
       AND viewer_policy_binding_generation IS NOT NULL AND viewer_policy_binding_generation > 0)
    );

-- +goose Down
ALTER TABLE paperboat.browser_access_sessions
  DROP CONSTRAINT browser_access_sessions_viewer_policy_team_authority_check,
  DROP COLUMN viewer_policy_binding_generation,
  DROP COLUMN viewer_policy_membership_generation,
  DROP COLUMN viewer_policy_team_generation,
  DROP COLUMN viewer_policy_team_id;

ALTER TABLE paperboat.browser_access_transactions
  DROP CONSTRAINT browser_access_transactions_viewer_policy_team_authority_check,
  DROP COLUMN viewer_policy_binding_generation,
  DROP COLUMN viewer_policy_membership_generation,
  DROP COLUMN viewer_policy_team_generation,
  DROP COLUMN viewer_policy_team_id;

CREATE OR REPLACE VIEW paperboat.current_tunnel_route_viewer_policy AS
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
