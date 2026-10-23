-- +goose Up
-- PLAN F19-07: every access to a machine (terminal, exec, managed SSH, files) is recorded in the
-- workspace that decided it, and every refusal with its reason. The workspace is derived here,
-- never supplied by the caller: a personal machine shared into a team is decided, and recorded,
-- in the team, so a member's access is in the team's log and not in the machine owner's.

-- machine_access_decision says in which workspace an access is decided and, when refused, why.
-- scope_set/scope_team are the request's workspace selector (scope_team NULL = personal). Reasons
-- use the names of `pb policy explain`, in its order: team_sso_required, machine_not_in_workspace
-- (not a member, team suspended or unpaid, machine not shared into the team), policy (the
-- workspace policy does not allow the capability), not_found (no such machine).
-- +goose StatementBegin
CREATE FUNCTION paperboat.machine_access_decision(actor text, machine text, capability text, scope_set boolean, scope_team text)
 RETURNS TABLE(workspace text, reason text, policy_version bigint)
 LANGUAGE plpgsql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
DECLARE
 m user_machines;
 team text;
BEGIN
 SELECT * INTO m FROM user_machines WHERE id=machine AND deleted_at IS NULL;
 IF NOT FOUND THEN
  RETURN QUERY SELECT 'personal:'||actor, 'not_found'::text, NULL::bigint;
  RETURN;
 END IF;
 IF scope_set THEN
  team := scope_team;
 ELSIF NOT (m.user_id=actor AND m.owner_team_id IS NULL) THEN
  -- No selector: the team that allows it, else a team the actor is in, else the owning team.
  SELECT b.team_id INTO team FROM team_resource_bindings b
  WHERE b.resource_kind='machine' AND b.resource_id=machine AND b.active
  ORDER BY team_machine_capability_allowed(actor, b.team_id, machine, capability) DESC,
   EXISTS(SELECT 1 FROM team_members tm WHERE tm.team_id=b.team_id AND tm.account_id=actor AND tm.active) DESC, b.team_id
  LIMIT 1;
  team := coalesce(team, m.owner_team_id);
  IF team IS NULL THEN
   RETURN QUERY SELECT 'personal:'||actor, 'machine_not_in_workspace'::text, NULL::bigint;
   RETURN;
  END IF;
 END IF;
 IF team IS NULL THEN
  RETURN QUERY SELECT 'personal:'||actor,
   CASE WHEN machine_capability_allowed(actor, machine, capability) AND m.user_id=actor AND m.owner_team_id IS NULL THEN NULL
    WHEN m.user_id=actor AND m.owner_team_id IS NULL AND m.revoked_at IS NULL THEN 'policy'
    ELSE 'machine_not_in_workspace' END,
   (SELECT p.version FROM workspace_policies p WHERE p.owner_account=actor);
  RETURN;
 END IF;
 RETURN QUERY SELECT 'team:'||team,
  CASE WHEN team_machine_capability_allowed(actor, team, machine, capability) THEN NULL
   WHEN NOT EXISTS(SELECT 1 FROM team_members tm WHERE tm.team_id=team AND tm.account_id=actor AND tm.active) THEN 'machine_not_in_workspace'
   WHEN NOT team_member_sso_ok(team, actor) THEN 'team_sso_required'
   WHEN NOT team_machine_bound_for(actor, team, machine) THEN 'machine_not_in_workspace'
   ELSE 'policy' END,
  (SELECT p.version FROM workspace_policies p WHERE p.team_id=team);
END $$;
-- +goose StatementEnd

-- Activity scope: as 071, plus machine_access events, scoped by machine_access_decision from the
-- event's own capability and workspace selector.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.snapshot_activity_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 -- Do not accept a caller-supplied activity scope.
 NEW.activity_workspace_id := NULL;
 NEW.activity_machine_id := NULL;
 NEW.activity_machine_name := NULL;
 NEW.activity_target_name := NULL;
 IF NEW.resource_type='machine_access' THEN
  SELECT d.workspace INTO NEW.activity_workspace_id FROM paperboat.machine_access_decision(NEW.actor_user_id, NEW.resource_id,
   NEW.metadata->>'capability', coalesce((NEW.metadata->>'scope_set')::boolean, false), NEW.metadata->>'scope_team') d;
  SELECT m.id,m.alias,m.alias INTO NEW.activity_machine_id,NEW.activity_machine_name,NEW.activity_target_name
  FROM paperboat.user_machines m WHERE m.id=NEW.resource_id;
 ELSIF NEW.resource_type IN ('machine','user_machine','environment') THEN
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

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.snapshot_activity_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
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
DROP FUNCTION paperboat.machine_access_decision(text, text, text, boolean, text);
