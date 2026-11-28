-- +goose Up
-- DDL only. On the Free plan a browser page load of a public preview or HTTP tunnel needs sign-in (the owner, or the
-- emails/domain the viewer policy lists); every other request (APIs, webhooks, WebSockets, subresources) still passes.
-- workspace_navigation_gated is the one plan test: a personal workspace without a paid Pro period, no running
-- public_browser operator grant, not riding only its own edges, and the publication not placed on a self-hosted edge (selfhost_placements). Team workspaces are never gated.
-- The tunnel viewer-policy view keeps a gated route public at the edge (the edge gates only navigations), whatever
-- non-public audience its policy names.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.billing_workspace_grants DROP CONSTRAINT billing_workspace_grants_kind_check;
ALTER TABLE paperboat.billing_workspace_grants ADD CONSTRAINT billing_workspace_grants_kind_check
    CHECK (kind IN ('bandwidth','throttle_lift','machines','publications','browser_terminals','public_tcp','seats','viewers_per_terminal','sso','scim','public_browser')) NOT VALID;

-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_navigation_gated(wanted_kind text, wanted_id text, wanted_account text, wanted_team text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT wanted_team IS NULL
  -- Only Paperboat-managed edges: a publication placed on the owner's own self-hosted edge is exempt on any plan.
  AND NOT EXISTS(SELECT 1 FROM paperboat.selfhost_placements sp WHERE sp.resource_kind=wanted_kind AND sp.resource_id=wanted_id)
  AND coalesce((SELECT w.product_code FROM paperboat.billing_workspaces w
   WHERE w.user_id=wanted_account AND w.team_id IS NULL AND paperboat.billing_workspace_paid_until(w)>now()),'') NOT LIKE 'pro-%'
  AND NOT EXISTS(SELECT 1 FROM paperboat.billing_workspace_grants g WHERE g.workspace_id='personal:'||wanted_account
   AND g.kind='public_browser' AND g.revoked_at IS NULL AND (g.ends_at IS NULL OR g.ends_at>now()))
  AND paperboat.workspace_edge_mode(wanted_account,NULL) IS DISTINCT FROM 'self-hosted-only';
$$;
-- +goose StatementEnd

CREATE OR REPLACE VIEW paperboat.current_tunnel_route_viewer_policy AS
  SELECT
    r.id AS route_id,
    CASE
      WHEN r.protocol = 'http' AND p.audience IS NOT NULL THEN
        CASE
          WHEN t.access_mode = 'public' AND p.audience = 'public' THEN 'public'
          WHEN t.access_mode = 'public' AND paperboat.workspace_navigation_gated('tunnel', t.id, t.account_id, t.owner_team_id) THEN 'public'
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
-- Forward only (zero users).
SELECT 1;
