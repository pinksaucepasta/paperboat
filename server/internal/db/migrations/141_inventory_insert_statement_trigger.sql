-- +goose Up
-- Inserting machines published the inventory topic once per inserted row to the row's whole
-- audience (every machine of the account), so one statement inserting n machines into an account
-- of m bumped n*m topic rows: a bulk insert of 4097 machines did ~8.4M upserts and never finished
-- (TestAccessorRouteDiscoveryOnPostgres). The insert trigger is now per statement: the union of the
-- inserted rows' audiences is computed once and each machine is bumped once. A machine has no team
-- resource bindings yet when it is inserted, so its audience is its account's machines and, for a
-- team-owned machine, the team members' machines. Updates keep the row trigger.
-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_machine_inventory_inserted() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM paperboat.publish_machine_topic(ARRAY(
    SELECT peer.id FROM paperboat.user_machines peer
     WHERE peer.deleted_at IS NULL AND peer.user_id IN (SELECT DISTINCT user_id FROM inserted)
    UNION
    SELECT peer.id FROM inserted i
     JOIN paperboat.team_members member ON member.team_id = i.owner_team_id AND member.active
     JOIN paperboat.user_machines peer ON peer.user_id = member.account_id AND peer.deleted_at IS NULL
     WHERE i.owner_team_id IS NOT NULL), 'inventory');
  RETURN NULL;
END $$;
-- +goose StatementEnd
DROP TRIGGER user_machines_publish_inventory_insert ON paperboat.user_machines;
CREATE TRIGGER user_machines_publish_inventory_insert
    AFTER INSERT ON paperboat.user_machines
    REFERENCING NEW TABLE AS inserted
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_machine_inventory_inserted();

-- +goose Down
DROP TRIGGER user_machines_publish_inventory_insert ON paperboat.user_machines;
DROP FUNCTION paperboat.publish_machine_inventory_inserted();
CREATE TRIGGER user_machines_publish_inventory_insert
    AFTER INSERT ON paperboat.user_machines
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_machine_inventory();
