-- +goose Up
-- Bulk changes published their topics once per changed row (F21-10): deactivating a team's 1,000 members upserted
-- the inventory topic 1,000 times, revoking a user's 1,000 access sessions upserted machine_access per session and
-- bumped the endpoint identities' generations per session, and replacing many machines' service snapshots
-- upserted machine_services per row for each row's whole audience. These triggers now run once per statement over
-- their transition tables: one upsert per (account or machine, topic) however many rows changed, and one update per
-- identity. What each publishes is unchanged. Transition tables cannot be combined with UPDATE OF column lists, so
-- each update trigger compares the old and new rows itself; a statement that changed no watched column publishes
-- nothing.
-- Prior art: migrations 141 and 168 (same rewrite for inserts and certificate revocations); PostgreSQL transition
-- tables (REFERENCING OLD/NEW TABLE) are the documented set-based form of a per-row trigger.

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_team_memberships() RETURNS trigger LANGUAGE plpgsql AS $$
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
DROP TRIGGER team_members_publish_insert ON paperboat.team_members;
DROP TRIGGER team_members_publish_update ON paperboat.team_members;
CREATE TRIGGER team_members_publish_insert AFTER INSERT ON paperboat.team_members
    REFERENCING NEW TABLE AS new_members
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_team_memberships();
CREATE TRIGGER team_members_publish_update AFTER UPDATE ON paperboat.team_members
    REFERENCING OLD TABLE AS old_members NEW TABLE AS new_members
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_team_memberships();

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_machine_access_sessions() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM paperboat.publish_machine_topic(ARRAY(SELECT DISTINCT user_machine_id FROM new_sessions), 'machine_access');
  ELSE
    PERFORM paperboat.publish_machine_topic(ARRAY(SELECT DISTINCT n.user_machine_id FROM new_sessions n
      JOIN old_sessions o ON o.id = n.id WHERE o.state IS DISTINCT FROM n.state), 'machine_access');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- The endpoint identities of the sessions' CLI sessions and machines move to a new configuration generation (and,
-- for a session that changed, a relay revocation generation), once per identity per statement.
-- +goose StatementBegin
CREATE FUNCTION paperboat.advance_peer_network_for_access_sessions() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  changed_ids text[];
BEGIN
  -- Only the branch taken is planned: an insert trigger has no old table.
  IF TG_OP = 'INSERT' THEN
    changed_ids := ARRAY(SELECT id FROM new_sessions);
  ELSE
    changed_ids := ARRAY(SELECT n.id FROM new_sessions n JOIN old_sessions o ON o.id = n.id
      WHERE o.state IS DISTINCT FROM n.state OR o.revoked_at IS DISTINCT FROM n.revoked_at OR o.expires_at IS DISTINCT FROM n.expires_at);
  END IF;
  IF cardinality(changed_ids) = 0 THEN
    RETURN NULL;
  END IF;
  WITH changed AS (
    SELECT n.user_id, n.cli_client_session_id, n.user_machine_id FROM new_sessions n WHERE n.id = ANY(changed_ids)),
  endpoints AS (
    SELECT c.user_id, c.cli_client_session_id AS endpoint_id FROM changed c
    UNION
    SELECT m.user_id, m.id FROM changed c JOIN paperboat.user_machines m ON m.id = c.user_machine_id)
  UPDATE paperboat.peer_network_identities n SET
    relay_revocation_generation = CASE WHEN TG_OP = 'INSERT' THEN relay_revocation_generation ELSE config_generation + 1 END,
    config_generation = config_generation + 1, updated_at = now()
  FROM endpoints e WHERE n.user_id = e.user_id AND n.endpoint_id = e.endpoint_id;
  RETURN NULL;
END $$;
-- +goose StatementEnd
DROP TRIGGER user_machine_access_sessions_publish_insert ON paperboat.user_machine_access_sessions;
DROP TRIGGER user_machine_access_sessions_publish_update ON paperboat.user_machine_access_sessions;
DROP TRIGGER user_machine_access_peer_network_generation ON paperboat.user_machine_access_sessions;
CREATE TRIGGER user_machine_access_sessions_publish_insert AFTER INSERT ON paperboat.user_machine_access_sessions
    REFERENCING NEW TABLE AS new_sessions
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_machine_access_sessions();
CREATE TRIGGER user_machine_access_sessions_publish_update AFTER UPDATE ON paperboat.user_machine_access_sessions
    REFERENCING OLD TABLE AS old_sessions NEW TABLE AS new_sessions
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_machine_access_sessions();
CREATE TRIGGER user_machine_access_peer_network_generation_insert AFTER INSERT ON paperboat.user_machine_access_sessions
    REFERENCING NEW TABLE AS new_sessions
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.advance_peer_network_for_access_sessions();
CREATE TRIGGER user_machine_access_peer_network_generation_update AFTER UPDATE ON paperboat.user_machine_access_sessions
    REFERENCING OLD TABLE AS old_sessions NEW TABLE AS new_sessions
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.advance_peer_network_for_access_sessions();

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_machine_services_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  changed_machines text[];
BEGIN
  IF TG_OP = 'INSERT' THEN
    changed_machines := ARRAY(SELECT DISTINCT machine_id FROM new_snapshots);
  ELSE
    changed_machines := ARRAY(SELECT DISTINCT n.machine_id FROM new_snapshots n JOIN old_snapshots o ON o.machine_id = n.machine_id
      WHERE o.services IS DISTINCT FROM n.services);
  END IF;
  IF cardinality(changed_machines) > 0 THEN
    PERFORM paperboat.publish_account_topic(ARRAY(
      SELECT DISTINCT a FROM unnest(changed_machines) m CROSS JOIN LATERAL unnest(paperboat.machine_audience_accounts(m)) a), 'machine_services');
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
DROP TRIGGER machine_service_snapshots_publish_insert ON paperboat.machine_service_snapshots;
DROP TRIGGER machine_service_snapshots_publish_update ON paperboat.machine_service_snapshots;
CREATE TRIGGER machine_service_snapshots_publish_insert AFTER INSERT ON paperboat.machine_service_snapshots
    REFERENCING NEW TABLE AS new_snapshots
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_machine_services_changed();
CREATE TRIGGER machine_service_snapshots_publish_update AFTER UPDATE ON paperboat.machine_service_snapshots
    REFERENCING OLD TABLE AS old_snapshots NEW TABLE AS new_snapshots
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_machine_services_changed();

-- +goose Down
DROP TRIGGER machine_service_snapshots_publish_update ON paperboat.machine_service_snapshots;
DROP TRIGGER machine_service_snapshots_publish_insert ON paperboat.machine_service_snapshots;
CREATE TRIGGER machine_service_snapshots_publish_insert
    AFTER INSERT ON paperboat.machine_service_snapshots
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_machine_services();
CREATE TRIGGER machine_service_snapshots_publish_update
    AFTER UPDATE OF services ON paperboat.machine_service_snapshots
    FOR EACH ROW WHEN (OLD.services IS DISTINCT FROM NEW.services)
    EXECUTE FUNCTION paperboat.publish_machine_services();
DROP FUNCTION paperboat.publish_machine_services_changed();

DROP TRIGGER user_machine_access_peer_network_generation_update ON paperboat.user_machine_access_sessions;
DROP TRIGGER user_machine_access_peer_network_generation_insert ON paperboat.user_machine_access_sessions;
DROP TRIGGER user_machine_access_sessions_publish_update ON paperboat.user_machine_access_sessions;
DROP TRIGGER user_machine_access_sessions_publish_insert ON paperboat.user_machine_access_sessions;
CREATE TRIGGER user_machine_access_peer_network_generation AFTER INSERT OR UPDATE OF state, revoked_at, expires_at ON paperboat.user_machine_access_sessions
    FOR EACH ROW EXECUTE FUNCTION paperboat.advance_peer_network_for_access_session();
CREATE TRIGGER user_machine_access_sessions_publish_insert
    AFTER INSERT ON paperboat.user_machine_access_sessions
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_machine_access_session();
CREATE TRIGGER user_machine_access_sessions_publish_update
    AFTER UPDATE OF state ON paperboat.user_machine_access_sessions
    FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION paperboat.publish_machine_access_session();
DROP FUNCTION paperboat.advance_peer_network_for_access_sessions();
DROP FUNCTION paperboat.publish_machine_access_sessions();

DROP TRIGGER team_members_publish_update ON paperboat.team_members;
DROP TRIGGER team_members_publish_insert ON paperboat.team_members;
CREATE TRIGGER team_members_publish_insert
    AFTER INSERT ON paperboat.team_members
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_team_membership();
CREATE TRIGGER team_members_publish_update
    AFTER UPDATE OF active, role ON paperboat.team_members
    FOR EACH ROW WHEN (OLD.active IS DISTINCT FROM NEW.active OR OLD.role IS DISTINCT FROM NEW.role)
    EXECUTE FUNCTION paperboat.publish_team_membership();
DROP FUNCTION paperboat.publish_team_memberships();
