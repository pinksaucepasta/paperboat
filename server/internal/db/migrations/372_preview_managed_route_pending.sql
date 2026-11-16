-- +goose Up
-- decoratePreviewEndpoints ran one view-heavy statement (preview_routes twice, node_workspace_edge_eligible per edge) for
-- every preview it returned: on every GET, list item and renewal. Planning it took 326 ms for 30 ms of execution (staging,
-- 170 live previews, 2026-11-16), so a machine's renewals and a create's ten status polls were planning-bound and 749 of
-- 972 previews lost their owner. As a PL/pgSQL function with force_generic_plan (the pattern of migration 296) the plan is
-- kept for the session. Body and result are the statement's third column, unchanged.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.preview_managed_route_pending(wanted_preview text)
 RETURNS boolean
 LANGUAGE plpgsql
 STABLE
 SET plan_cache_mode TO 'force_generic_plan'
AS $function$
DECLARE
 lease_account text;
 lease_team text;
BEGIN
 SELECT account_id,owner_team_id INTO lease_account,lease_team FROM paperboat.preview_leases WHERE id=wanted_preview;
 IF NOT FOUND THEN
  RETURN false;
 END IF;
 RETURN paperboat.workspace_edge_mode(lease_account,lease_team)='managed-only' AND (
 NOT EXISTS (SELECT 1 FROM paperboat.preview_routes a WHERE a.preview_id=wanted_preview AND a.account_id=lease_account
  AND a.state='ready' AND a.edge_ready AND a.origin_ready AND a.expires_at>now())
 OR EXISTS (SELECT 1 FROM paperboat.control_tunnel_nodes n
  WHERE n.state='ready' AND n.ready AND n.drain_deadline IS NULL AND n.roles @> ARRAY['edge']::text[]
  AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now() AND n.public_ingress_verified_at IS NOT NULL
  AND paperboat.node_workspace_edge_eligible(n.id,lease_account,lease_team)
  AND NOT EXISTS (SELECT 1 FROM paperboat.preview_routes a WHERE a.preview_id=wanted_preview AND a.account_id=lease_account
   AND a.edge_node_id=n.id AND a.edge_process_epoch=n.process_epoch
   AND a.state='ready' AND a.edge_ready AND a.origin_ready AND a.expires_at>now())));
END
$function$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS paperboat.preview_managed_route_pending(text);
