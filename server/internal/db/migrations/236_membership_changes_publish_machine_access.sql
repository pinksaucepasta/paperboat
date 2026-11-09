-- +goose Up
-- A removed member's devices learned of it only through the inventory topic (migration 193), whose refresh is spaced
-- 15 s, so they dropped the team machines' scopes up to 15 s late. A membership change now also moves the account's
-- machine_access topic (the fast path of migrations 209 and 235). A deleted membership row publishes the same as a
-- deactivated one.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_memberships() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE changed text[];
BEGIN
  IF TG_OP = 'INSERT' THEN
    changed := ARRAY(SELECT DISTINCT account_id FROM new_members);
  ELSIF TG_OP = 'DELETE' THEN
    changed := ARRAY(SELECT DISTINCT account_id FROM old_members);
  ELSE
    changed := ARRAY(SELECT DISTINCT n.account_id FROM new_members n
      JOIN old_members o ON o.team_id = n.team_id AND o.account_id = n.account_id
      WHERE o.active IS DISTINCT FROM n.active OR o.role IS DISTINCT FROM n.role);
  END IF;
  PERFORM paperboat.publish_account_topic(changed, 'inventory');
  PERFORM paperboat.publish_account_topic(changed, 'machine_access');
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER team_members_publish_delete AFTER DELETE ON paperboat.team_members
    REFERENCING OLD TABLE AS old_members
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_team_memberships();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER team_members_publish_delete ON paperboat.team_members;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_memberships() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT account_id FROM new_members), 'inventory');
  ELSE
    PERFORM paperboat.publish_account_topic(ARRAY(SELECT DISTINCT n.account_id FROM new_members n
      JOIN old_members o ON o.team_id = n.team_id AND o.account_id = n.account_id
      WHERE o.active IS DISTINCT FROM n.active OR o.role IS DISTINCT FROM n.role), 'inventory');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
