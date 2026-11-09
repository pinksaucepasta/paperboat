-- +goose Up
-- A team's paid entitlement ending (or starting) decides every member's access to its machines
-- (team_machine_capability_allowed), but fence_team_paid_entitlement_change only bumped teams.generation: neither the
-- hosts' peer_network/machine_access topics nor the members' account topics moved, so signed documents kept the
-- scopes until the next renewal or fallback. It now also publishes both (migrations 196/206 helpers for hosts, 207/209
-- for members), and only when the paid flag flips, so a billing row update that keeps it publishes nothing.
-- The flag is the one team_paid_entitled uses (renewal grace counts as paid, migration 078); the baseline function
-- looked at state alone and missed a flip between active and grace.
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
  old_paid:=OLD.team_id IS NOT NULL AND OLD.state IN ('active','trialing') AND OLD.period_start IS NOT NULL AND OLD.period_start<=now() AND OLD.period_end IS NOT NULL AND now()<OLD.period_end;
 END IF;
 IF TG_OP<>'DELETE' THEN
  team:=coalesce(NEW.team_id,team);
  new_paid:=NEW.team_id IS NOT NULL AND NEW.state IN ('active','trialing') AND NEW.period_start IS NOT NULL AND NEW.period_start<=now() AND NEW.period_end IS NOT NULL AND now()<NEW.period_end;
 END IF;
 IF team IS NOT NULL AND old_paid IS DISTINCT FROM new_paid THEN
  UPDATE teams SET generation=generation+1 WHERE team_id=team;
 END IF;
 RETURN NULL;
END $$;
-- +goose StatementEnd
