-- +goose Up
-- A team's paid entitlement starting or ending turns its recording policy on or off (shared/recording Policy.Enabled),
-- so the flip also moves the recording_policy topic of the team's machines (only teams with a recording key publish).
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.fence_team_paid_entitlement_change() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
DECLARE
 team text;
 old_paid boolean := false;
 new_paid boolean := false;
BEGIN
 IF TG_OP<>'INSERT' THEN
  team:=OLD.team_id;
  old_paid:=coalesce(OLD.team_id IS NOT NULL AND OLD.user_id IS NULL AND OLD.period_start<=now() AND now()<billing_workspace_paid_until(OLD),false);
 END IF;
 IF TG_OP<>'DELETE' THEN
  team:=coalesce(NEW.team_id,team);
  new_paid:=coalesce(NEW.team_id IS NOT NULL AND NEW.user_id IS NULL AND NEW.period_start<=now() AND now()<billing_workspace_paid_until(NEW),false);
 END IF;
 IF team IS NOT NULL AND old_paid IS DISTINCT FROM new_paid THEN
  UPDATE teams SET generation=generation+1 WHERE team_id=team;
  PERFORM publish_team_hosts_peer_network(team);
  PERFORM publish_team_members_inventory(team);
  PERFORM publish_team_recording_policy(ARRAY[team]);
 END IF;
 RETURN NULL;
END $$;
-- +goose StatementEnd


-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.fence_team_paid_entitlement_change() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
DECLARE
 team text;
 old_paid boolean := false;
 new_paid boolean := false;
BEGIN
 IF TG_OP<>'INSERT' THEN
  team:=OLD.team_id;
  old_paid:=coalesce(OLD.team_id IS NOT NULL AND OLD.user_id IS NULL AND OLD.period_start<=now() AND now()<billing_workspace_paid_until(OLD),false);
 END IF;
 IF TG_OP<>'DELETE' THEN
  team:=coalesce(NEW.team_id,team);
  new_paid:=coalesce(NEW.team_id IS NOT NULL AND NEW.user_id IS NULL AND NEW.period_start<=now() AND now()<billing_workspace_paid_until(NEW),false);
 END IF;
 IF team IS NOT NULL AND old_paid IS DISTINCT FROM new_paid THEN
  UPDATE teams SET generation=generation+1 WHERE team_id=team;
  PERFORM publish_team_hosts_peer_network(team);
  PERFORM publish_team_members_inventory(team);
 END IF;
 RETURN NULL;
END $$;
-- +goose StatementEnd

