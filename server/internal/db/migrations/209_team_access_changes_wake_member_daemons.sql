-- +goose Up
-- F9-04: the members' account inventory (migration 207) refreshes their daemons only within the inventory event
-- spacing (15 s, a server-load bound for ordinary machine changes), so a member reached a machine just shared with
-- them 12-31 s after the grant. Access changes are rare: they also move the members' machine_access account topic,
-- on which the daemon refreshes its machine list within a second.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_members_inventory(wanted_team text) RETURNS void
    LANGUAGE plpgsql AS $$
DECLARE members text[];
BEGIN
  members := coalesce((SELECT array_agg(m.account_id) FROM paperboat.team_members m
   WHERE m.team_id = wanted_team AND m.active), '{}'::text[]);
  PERFORM paperboat.publish_account_topic(members, 'inventory');
  PERFORM paperboat.publish_account_topic(members, 'machine_access');
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_members_inventory(wanted_team text) RETURNS void
    LANGUAGE sql AS $$
 SELECT paperboat.publish_account_topic(coalesce((SELECT array_agg(m.account_id) FROM paperboat.team_members m
  WHERE m.team_id = wanted_team AND m.active), '{}'::text[]), 'inventory');
$$;
-- +goose StatementEnd
