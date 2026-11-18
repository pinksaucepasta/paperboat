-- +goose Up
-- Deleting a team at its plan ceiling (12,001 bindings) took 5.4 s unloaded, holding locks, because three row-level
-- triggers on team_resource_bindings ran once per binding: team_machine_access_binding revoked the team's access
-- sessions (the same UPDATE over the whole team, 12,001 times), terminal_access_binding did the same per terminal
-- session, and team_resource_bindings_publish_host_peer_network_insert bumped one machine topic per binding. They
-- now run once per statement over transition tables: the team access revocation once over the statement's teams,
-- the terminal revocation once over its (team, session) pairs, the topic publish once over its machine ids.
-- What each does is unchanged for one row. A bulk statement bumps a machine's topic once instead of once per row
-- (the intent of migration 193). An update revokes over the old and new rows' pairs, a superset of the old row-level
-- WHEN filter: the revocation only touches sessions whose permission is already invalid, so a wider scope never
-- revokes a valid session.
-- The functions shared with other tables (revoke_invalid_team_machine_access, revoke_invalid_terminal_session_access,
-- publish_binding_host_peer_network) stay for them; the UPDATE OF active, generation host publish stays row-level
-- (its WHEN clause cannot be a transition-table filter and it fires on a changed flag only).
-- Prior art: migrations 193 and 365 (the same rewrite for topic and peer policy triggers); PostgreSQL transition
-- tables. This table's DDL only; none of its triggers touch the shared 443 name tables.
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE FUNCTION paperboat.revoke_invalid_team_machine_access_bindings() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE affected_teams text[];
BEGIN
 IF TG_OP = 'DELETE' THEN
  affected_teams := ARRAY(SELECT DISTINCT team_id FROM old_bindings);
 ELSE
  affected_teams := ARRAY(SELECT DISTINCT team_id FROM new_bindings);
 END IF;
 IF cardinality(affected_teams) = 0 THEN RETURN NULL; END IF;
 UPDATE user_machine_access_sessions a SET state='revoked',revoked_at=now(),updated_at=now(),revocation_reason='machine_permission_revoked'
 WHERE a.state='active' AND a.team_id = ANY(affected_teams) AND (EXISTS(
  SELECT 1 FROM unnest(a.capabilities) capability
  WHERE CASE WHEN a.machine_service_port IS NOT NULL THEN NOT machine_service_session_allowed(a.id) WHEN a.team_id IS NOT NULL THEN NOT team_machine_capability_allowed(a.user_id,a.team_id,a.user_machine_id,capability)
   ELSE NOT EXISTS(SELECT 1 FROM user_machines m WHERE m.id=a.user_machine_id AND m.user_id=a.user_id AND m.owner_team_id IS NULL AND m.revoked_at IS NULL AND m.deleted_at IS NULL) END)
 OR EXISTS(SELECT 1 FROM user_machines m WHERE m.id=a.user_machine_id AND
  (m.state IN ('revoked','disconnected','deleted')
   OR (a.capabilities && ARRAY['terminal','exec'] AND NOT m.configured_capabilities @> ARRAY['terminal_host'])
   OR ('managed_ssh'=ANY(a.capabilities) AND NOT m.configured_capabilities @> ARRAY['ssh_host'])
   OR ('files'=ANY(a.capabilities) AND NOT m.configured_capabilities @> ARRAY['file_receive']))));
 RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.revoke_invalid_terminal_session_access_bindings() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'DELETE' THEN
  UPDATE user_machine_access_sessions a SET state='revoked',revoked_at=now(),updated_at=now(),revocation_reason='terminal_session_permission_revoked'
  FROM (SELECT DISTINCT team_id, resource_id FROM old_bindings WHERE resource_kind='terminal_session') p
  WHERE a.state='active' AND a.terminal_role IN ('viewer','interactive') AND a.team_id=p.team_id AND a.terminal_session_id=p.resource_id
   AND terminal_session_role(a.user_id,a.terminal_session_id) IS DISTINCT FROM a.terminal_role;
 ELSE
  UPDATE user_machine_access_sessions a SET state='revoked',revoked_at=now(),updated_at=now(),revocation_reason='terminal_session_permission_revoked'
  FROM (SELECT team_id, resource_id FROM old_bindings WHERE resource_kind='terminal_session'
        UNION SELECT team_id, resource_id FROM new_bindings WHERE resource_kind='terminal_session') p
  WHERE a.state='active' AND a.terminal_role IN ('viewer','interactive') AND a.team_id=p.team_id AND a.terminal_session_id=p.resource_id
   AND terminal_session_role(a.user_id,a.terminal_session_id) IS DISTINCT FROM a.terminal_role;
 END IF;
 RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION paperboat.publish_binding_hosts_peer_network() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE hosts text[];
BEGIN
 IF TG_OP = 'DELETE' THEN
  hosts := ARRAY(SELECT DISTINCT resource_id FROM old_bindings WHERE resource_kind='machine');
 ELSE
  hosts := ARRAY(SELECT DISTINCT resource_id FROM new_bindings WHERE resource_kind='machine');
 END IF;
 IF cardinality(hosts) > 0 THEN
  PERFORM paperboat.publish_machine_topic(hosts, 'peer_network');
  PERFORM paperboat.publish_machine_topic(hosts, 'machine_access');
 END IF;
 RETURN NULL;
END $$;
-- +goose StatementEnd

DROP TRIGGER team_machine_access_binding ON paperboat.team_resource_bindings;
DROP TRIGGER terminal_access_binding ON paperboat.team_resource_bindings;
DROP TRIGGER team_resource_bindings_publish_host_peer_network_insert ON paperboat.team_resource_bindings;

CREATE TRIGGER team_machine_access_binding_delete AFTER DELETE ON paperboat.team_resource_bindings
    REFERENCING OLD TABLE AS old_bindings
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.revoke_invalid_team_machine_access_bindings();
CREATE TRIGGER team_machine_access_binding_update AFTER UPDATE ON paperboat.team_resource_bindings
    REFERENCING NEW TABLE AS new_bindings
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.revoke_invalid_team_machine_access_bindings();
CREATE TRIGGER terminal_access_binding_delete AFTER DELETE ON paperboat.team_resource_bindings
    REFERENCING OLD TABLE AS old_bindings
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.revoke_invalid_terminal_session_access_bindings();
CREATE TRIGGER terminal_access_binding_update AFTER UPDATE ON paperboat.team_resource_bindings
    REFERENCING OLD TABLE AS old_bindings NEW TABLE AS new_bindings
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.revoke_invalid_terminal_session_access_bindings();
CREATE TRIGGER team_resource_bindings_publish_host_peer_network_insert AFTER INSERT ON paperboat.team_resource_bindings
    REFERENCING NEW TABLE AS new_bindings
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_binding_hosts_peer_network();
CREATE TRIGGER team_resource_bindings_publish_host_peer_network_delete AFTER DELETE ON paperboat.team_resource_bindings
    REFERENCING OLD TABLE AS old_bindings
    FOR EACH STATEMENT EXECUTE FUNCTION paperboat.publish_binding_hosts_peer_network();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER team_machine_access_binding_delete ON paperboat.team_resource_bindings;
DROP TRIGGER team_machine_access_binding_update ON paperboat.team_resource_bindings;
DROP TRIGGER terminal_access_binding_delete ON paperboat.team_resource_bindings;
DROP TRIGGER terminal_access_binding_update ON paperboat.team_resource_bindings;
DROP TRIGGER team_resource_bindings_publish_host_peer_network_insert ON paperboat.team_resource_bindings;
DROP TRIGGER team_resource_bindings_publish_host_peer_network_delete ON paperboat.team_resource_bindings;
CREATE TRIGGER team_machine_access_binding AFTER DELETE OR UPDATE ON paperboat.team_resource_bindings
    FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_invalid_team_machine_access();
CREATE TRIGGER terminal_access_binding AFTER DELETE OR UPDATE ON paperboat.team_resource_bindings
    FOR EACH ROW WHEN (old.resource_kind = 'terminal_session') EXECUTE FUNCTION paperboat.revoke_invalid_terminal_session_access();
CREATE TRIGGER team_resource_bindings_publish_host_peer_network_insert AFTER INSERT OR DELETE ON paperboat.team_resource_bindings
    FOR EACH ROW EXECUTE FUNCTION paperboat.publish_binding_host_peer_network();
DROP FUNCTION paperboat.revoke_invalid_team_machine_access_bindings();
DROP FUNCTION paperboat.revoke_invalid_terminal_session_access_bindings();
DROP FUNCTION paperboat.publish_binding_hosts_peer_network();
