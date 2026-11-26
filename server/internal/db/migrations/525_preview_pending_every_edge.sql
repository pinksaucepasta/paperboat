-- +goose Up
-- A managed preview is published (pending flag cleared) only once every live edge holding its route serves it:
-- managed DNS resolves to every live edge and edges do not forward, so with one edge ready (482) a visitor's first
-- request on the other edge answered 404/502. Function only: no table DDL, no data change.
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
 OR NOT EXISTS (SELECT 1 FROM paperboat.endpoints e JOIN paperboat.endpoint_routes er ON er.endpoint_id=e.id
  WHERE e.preview_lease_id=wanted_preview AND paperboat.endpoint_route_ready(er.id,now(),true)));
END
$function$;
-- +goose StatementEnd

-- +goose Down
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
 OR NOT EXISTS (SELECT 1 FROM paperboat.endpoints e JOIN paperboat.endpoint_routes er ON er.endpoint_id=e.id
  WHERE e.preview_lease_id=wanted_preview AND paperboat.endpoint_route_ready(er.id,now(),false)));
END
$function$;
-- +goose StatementEnd
