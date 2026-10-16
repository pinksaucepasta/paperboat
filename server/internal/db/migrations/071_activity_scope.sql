-- +goose Up
-- Snapshot exact resource ownership when an event is written. Historical events
-- without a retained scope are omitted, never attributed by today's ownership.
ALTER TABLE paperboat.audit_events
 ADD COLUMN activity_workspace_id text,
 ADD COLUMN activity_machine_id text,
 ADD COLUMN activity_machine_name text,
 ADD COLUMN activity_target_name text;
CREATE INDEX audit_events_activity_workspace_cursor
 ON paperboat.audit_events(activity_workspace_id,cursor_sequence DESC)
 WHERE activity_workspace_id IS NOT NULL;
-- +goose StatementBegin
CREATE FUNCTION paperboat.snapshot_activity_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 -- Do not accept a caller-supplied activity scope.
 NEW.activity_workspace_id := NULL;
 NEW.activity_machine_id := NULL;
 NEW.activity_machine_name := NULL;
 NEW.activity_target_name := NULL;
 IF NEW.resource_type IN ('machine','user_machine','environment') THEN
  SELECT coalesce('team:'||m.owner_team_id,'personal:'||m.user_id),m.id,m.alias,m.alias
  INTO NEW.activity_workspace_id,NEW.activity_machine_id,NEW.activity_machine_name,NEW.activity_target_name
  FROM paperboat.user_machines m WHERE
  (NEW.resource_type='environment' AND m.environment_id=NEW.resource_id)
  OR (NEW.resource_type<>'environment' AND m.id=NEW.resource_id);
 ELSIF NEW.resource_type='terminal_session' THEN
  SELECT coalesce('team:'||t.owner_team_id,'personal:'||t.owner_account),m.id,m.alias,coalesce(t.title,t.name)
  INTO NEW.activity_workspace_id,NEW.activity_machine_id,NEW.activity_machine_name,NEW.activity_target_name
  FROM paperboat.user_machine_terminal_sessions t
  JOIN paperboat.user_machines m ON m.id=t.user_machine_id WHERE t.id=NEW.resource_id;
 ELSIF NEW.resource_type='preview_lease' THEN
  SELECT coalesce('team:'||p.owner_team_id,'personal:'||p.account_id),m.id,m.alias,p.endpoint
  INTO NEW.activity_workspace_id,NEW.activity_machine_id,NEW.activity_machine_name,NEW.activity_target_name
  FROM paperboat.preview_leases p LEFT JOIN paperboat.user_machines m ON m.id=p.owner_machine_id WHERE p.id=NEW.resource_id;
 ELSIF NEW.resource_type='tunnel' THEN
  SELECT coalesce('team:'||t.owner_team_id,'personal:'||t.account_id),m.id,m.alias,t.name
  INTO NEW.activity_workspace_id,NEW.activity_machine_id,NEW.activity_machine_name,NEW.activity_target_name
  FROM paperboat.tunnels t LEFT JOIN paperboat.user_machines m ON m.id=t.created_by_host_id WHERE t.id=NEW.resource_id;
 ELSIF NEW.resource_type='team' THEN
  SELECT 'team:'||t.team_id,t.display_name INTO NEW.activity_workspace_id,NEW.activity_target_name
  FROM paperboat.teams t WHERE t.team_id=NEW.resource_id;
 ELSIF NEW.resource_type='user' THEN
  SELECT 'personal:'||u.id INTO NEW.activity_workspace_id FROM paperboat.users u WHERE u.id=NEW.resource_id;
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER audit_events_activity_scope BEFORE INSERT ON paperboat.audit_events
 FOR EACH ROW EXECUTE FUNCTION paperboat.snapshot_activity_scope();
-- +goose Down
-- Audit scope is retained with its append-only event.
