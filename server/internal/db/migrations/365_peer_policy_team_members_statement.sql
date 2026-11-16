-- +goose Up
-- Deactivating a team's 1,000 members in one statement ran the row-level peer_policy_update trigger (334) 1,000 times,
-- each computing peer_policy_team_keys over the whole team: O(N^2), past the statement timeout. The update trigger now
-- runs once per statement over its transition tables, like the insert and delete ones. It names the teams and the
-- accounts of the rows whose watched columns changed, before and after (a member leaving or moving is no longer in
-- the team's own rows); a statement that changed no watched column publishes nothing.
-- The other row-level peer policy triggers (339-359) stay row level: their per-row work does not grow with the
-- statement (one team row, one billing or key row per team, user_machines updated by id), and a transition table on
-- the hot user_machines would be built for every presence write.
-- Prior art: migration 193 (same rewrite for the team_members topic triggers); PostgreSQL transition tables.
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_peer_policy_team_members() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE teams text[]; accounts text[];
BEGIN
  -- Changed rows: each side's versions the other side lacks (team_id or account_id may change too).
  SELECT array_agg(DISTINCT team_id), array_agg(DISTINCT 'account:' || account_id) INTO teams, accounts FROM (
    (SELECT team_id, account_id, active, role, membership_generation FROM new_rows
     EXCEPT SELECT team_id, account_id, active, role, membership_generation FROM old_rows)
    UNION
    (SELECT team_id, account_id, active, role, membership_generation FROM old_rows
     EXCEPT SELECT team_id, account_id, active, role, membership_generation FROM new_rows)) changed;
  IF teams IS NOT NULL THEN
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_team_keys(teams) || accounts);
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- team_members no longer uses the row function: it serves teams only.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_peer_policy_team_row() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_team_keys(ARRAY[coalesce(NEW.team_id, OLD.team_id)]) || ARRAY['account:' || OLD.owner_account, 'account:' || NEW.owner_account]);
  RETURN NULL;
END $$;
-- +goose StatementEnd

DROP TRIGGER peer_policy_update ON paperboat.team_members;
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.team_members REFERENCING NEW TABLE AS new_rows OLD TABLE AS old_rows
 FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_peer_policy_team_members();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER peer_policy_update ON paperboat.team_members;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_peer_policy_team_row() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_TABLE_NAME = 'teams' THEN
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_team_keys(ARRAY[coalesce(NEW.team_id, OLD.team_id)]) || ARRAY['account:' || OLD.owner_account, 'account:' || NEW.owner_account]);
  ELSE
    PERFORM paperboat.publish_peer_policy(paperboat.peer_policy_team_keys(ARRAY[NEW.team_id, OLD.team_id]) || ARRAY['account:' || OLD.account_id, 'account:' || NEW.account_id]);
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER peer_policy_update AFTER UPDATE ON paperboat.team_members FOR EACH ROW
 WHEN ((OLD.team_id, OLD.account_id, OLD.active, OLD.role, OLD.membership_generation) IS DISTINCT FROM (NEW.team_id, NEW.account_id, NEW.active, NEW.role, NEW.membership_generation))
 EXECUTE FUNCTION paperboat.publish_peer_policy_team_row();
DROP FUNCTION paperboat.publish_peer_policy_team_members();
