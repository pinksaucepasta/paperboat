-- +goose Up
ALTER TABLE paperboat.teams ADD CONSTRAINT teams_personal_workspace_reserved CHECK (team_id <> 'personal');
-- Workspace ownership is independent of the resource's durable account principal.
ALTER TABLE paperboat.tunnels ADD COLUMN owner_team_id text REFERENCES paperboat.teams(team_id);
ALTER TABLE paperboat.preview_leases ADD COLUMN owner_team_id text REFERENCES paperboat.teams(team_id);
ALTER TABLE paperboat.user_machine_terminal_sessions ADD COLUMN owner_team_id text REFERENCES paperboat.teams(team_id);
ALTER TABLE paperboat.tunnels DROP CONSTRAINT tunnels_account_id_name_key;
CREATE UNIQUE INDEX tunnels_account_id_name_personal ON paperboat.tunnels(account_id,name) WHERE owner_team_id IS NULL;
CREATE UNIQUE INDEX tunnels_account_id_name_team ON paperboat.tunnels(owner_team_id,name) WHERE owner_team_id IS NOT NULL;
CREATE INDEX tunnels_owner_team ON paperboat.tunnels(owner_team_id) WHERE owner_team_id IS NOT NULL;
CREATE INDEX preview_leases_owner_team ON paperboat.preview_leases(owner_team_id) WHERE owner_team_id IS NOT NULL;
-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_publisher_host_capability_allowed(actor text, team text, machine text, capability text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT capability=ANY(ARRAY['preview_manage','tunnel_manage'])
 AND EXISTS(SELECT 1 FROM users WHERE id=actor AND status='active') AND CASE WHEN team IS NULL THEN EXISTS(
  SELECT 1 FROM user_machines m WHERE m.id=machine AND m.user_id=actor AND m.owner_team_id IS NULL
   AND m.deleted_at IS NULL AND m.revoked_at IS NULL
 ) ELSE EXISTS(SELECT 1 FROM teams t JOIN team_members member USING(team_id) JOIN user_machines m ON m.id=machine
 WHERE t.team_id=team AND t.deleted_at IS NULL AND NOT t.suspended AND member.account_id=actor AND member.active
 AND ((m.user_id=actor AND m.owner_team_id IS NULL AND m.deleted_at IS NULL AND m.revoked_at IS NULL)
 OR team_machine_capability_allowed(actor,team,machine,capability))) END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_preview_management_allowed(actor text, team text, preview text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM preview_leases p JOIN user_machines m ON m.id=p.owner_machine_id
 WHERE p.id=preview
 AND (CASE WHEN team IS NULL THEN p.owner_team_id IS NULL AND p.account_id=actor AND m.owner_team_id IS NULL AND m.user_id=actor
 ELSE (m.configured_capabilities @> ARRAY['preview_launch']::text[] OR (p.account_id=actor AND m.user_id=actor AND m.owner_team_id IS NULL)) AND (p.access_mode IN ('private','team') OR (p.account_id=actor AND m.user_id=actor AND m.owner_team_id IS NULL)) AND (p.owner_team_id=team OR (p.owner_team_id IS NULL AND EXISTS(
  SELECT 1 FROM team_resource_bindings b WHERE b.team_id=team AND b.resource_kind='preview' AND b.resource_id=p.id AND b.owner_account=p.account_id AND b.active
 ))) END) AND (team IS NULL OR workspace_publisher_host_capability_allowed(actor,team,m.id,'preview_manage')));
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_tunnel_management_allowed(actor text, team text, tunnel text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM tunnels t JOIN user_machines m ON m.id=t.created_by_host_id
 WHERE t.id=tunnel
 AND (CASE WHEN team IS NULL THEN t.owner_team_id IS NULL AND t.account_id=actor AND m.owner_team_id IS NULL AND m.user_id=actor
 ELSE (m.configured_capabilities @> ARRAY['preview_launch']::text[] OR (t.account_id=actor AND m.user_id=actor AND m.owner_team_id IS NULL)) AND (t.access_mode IN ('private','team') OR (t.account_id=actor AND m.user_id=actor AND m.owner_team_id IS NULL)) AND (t.owner_team_id=team OR (t.owner_team_id IS NULL AND EXISTS(
  SELECT 1 FROM team_resource_bindings b WHERE b.team_id=team AND b.resource_kind='tunnel' AND b.resource_id=t.id AND b.owner_account=t.account_id AND b.active
 ))) END) AND (team IS NULL OR workspace_publisher_host_capability_allowed(actor,team,m.id,'tunnel_manage'))
 AND (team IS NULL OR NOT EXISTS(SELECT 1 FROM tunnel_connectors c LEFT JOIN user_machines other ON other.id=c.host_id
 WHERE c.tunnel_id=t.id AND c.revoked_at IS NULL AND (other.id IS NULL OR NOT workspace_publisher_host_capability_allowed(actor,team,other.id,'tunnel_manage') OR (NOT other.configured_capabilities @> ARRAY['preview_launch']::text[] AND NOT (other.id=m.id AND t.account_id=actor AND m.user_id=actor AND m.owner_team_id IS NULL))))));
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.preview_management_allowed(actor text, preview text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM preview_leases p JOIN user_machines m ON m.id=p.owner_machine_id WHERE p.id=preview AND (
 (p.owner_team_id IS NOT NULL AND workspace_preview_management_allowed(actor,p.owner_team_id,p.id))
 OR (p.owner_team_id IS NULL AND ((p.account_id=actor AND m.owner_team_id IS NULL AND m.user_id=actor)
 OR (p.access_mode IN ('private','team') AND m.configured_capabilities @> ARRAY['preview_launch']::text[] AND machine_capability_allowed(actor,m.id,'preview_manage'))))));
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.tunnel_management_allowed(actor text, tunnel text) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM tunnels t JOIN user_machines m ON m.id=t.created_by_host_id WHERE t.id=tunnel AND (
 (t.owner_team_id IS NOT NULL AND workspace_tunnel_management_allowed(actor,t.owner_team_id,t.id))
 OR (t.owner_team_id IS NULL AND ((t.account_id=actor AND m.owner_team_id IS NULL AND m.user_id=actor)
 OR (t.access_mode IN ('private','team') AND t.deleted_at IS NULL AND m.configured_capabilities @> ARRAY['preview_launch']::text[]
 AND machine_capability_allowed(actor,m.id,'tunnel_manage') AND NOT EXISTS(SELECT 1 FROM tunnel_connectors c LEFT JOIN user_machines other ON other.id=c.host_id
 WHERE c.tunnel_id=t.id AND c.revoked_at IS NULL AND (other.id IS NULL OR NOT machine_capability_allowed(actor,other.id,'tunnel_manage') OR NOT other.configured_capabilities @> ARRAY['preview_launch']::text[])))))));
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.publisher_workspace_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.owner_team_id IS DISTINCT FROM OLD.owner_team_id THEN
  RAISE EXCEPTION 'publisher workspace ownership is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER tunnel_workspace_immutable BEFORE UPDATE OF owner_team_id ON paperboat.tunnels FOR EACH ROW EXECUTE FUNCTION paperboat.publisher_workspace_immutable();
CREATE TRIGGER preview_workspace_immutable BEFORE UPDATE OF owner_team_id ON paperboat.preview_leases FOR EACH ROW EXECUTE FUNCTION paperboat.publisher_workspace_immutable();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.machine_service_authorities(actor text, machine text, port integer) RETURNS TABLE(source_kind text, source_id text, source_generation bigint, team_id text)
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 WITH host AS (
 SELECT m.* FROM user_machines m JOIN users owner ON owner.id=m.user_id AND owner.status='active'
 WHERE m.id=machine AND port BETWEEN 1 AND 65535 AND m.revoked_at IS NULL AND m.deleted_at IS NULL
 AND m.state NOT IN ('pending','revoked','deleted') AND m.seat_state='occupied'
 AND EXISTS(SELECT 1 FROM users u WHERE u.id=actor AND u.status='active')
 ), explicit AS (
 SELECT 'lazy_policy'::text kind,p.id,p.generation,p.account_id,p.access_mode,NULL::text owner_team_id FROM host m JOIN lazy_access_policies p ON p.machine_id=m.id
 WHERE p.authorization_source='explicit' AND p.installation_generation=m.installation_generation AND p.deleted_at IS NULL AND p.expires_at>now()
 AND p.target_address IN ('127.0.0.1:'||port,'[::1]:'||port)
 UNION ALL
 SELECT 'tunnel',t.id,r.generation,t.account_id,t.access_mode,t.owner_team_id FROM host m JOIN tunnels t ON t.created_by_host_id=m.id JOIN tunnel_routes r ON r.tunnel_id=t.id
 WHERE t.account_id=m.user_id AND t.desired_state='active' AND t.deleted_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>now())
 AND r.protocol='private_tcp' AND r.desired_state='active' AND r.deleted_at IS NULL AND r.origin_address IN ('127.0.0.1:'||port,'[::1]:'||port)
 )
 SELECT e.kind,e.id,e.generation,NULL::text FROM explicit e CROSS JOIN host m
 WHERE m.owner_team_id IS NULL AND m.user_id=actor AND e.account_id=m.user_id AND e.access_mode='private' AND e.owner_team_id IS NULL
 UNION ALL
 SELECT e.kind,e.id,e.generation,b.team_id FROM explicit e CROSS JOIN host m
 JOIN team_resource_bindings b ON b.resource_kind=e.kind AND b.resource_id=e.id AND b.owner_account=m.user_id AND b.active
 JOIN teams t ON t.team_id=b.team_id AND t.deleted_at IS NULL
 JOIN team_members member ON member.team_id=b.team_id AND member.account_id=actor AND member.active
 JOIN team_resource_grants g ON g.team_id=b.team_id AND g.account_id=actor AND g.resource_kind=e.kind AND g.resource_id=e.id AND g.active AND g.permission IN ('use','manage')
 WHERE team_paid_entitled(b.team_id) AND (e.owner_team_id IS NULL OR e.owner_team_id=b.team_id) AND (e.owner_team_id=b.team_id OR (m.owner_team_id IS NULL AND e.access_mode='team') OR m.owner_team_id=b.team_id)
 UNION ALL
 SELECT 'automatic',m.id,m.machine_service_policy_generation,NULL::text FROM host m
 WHERE port>=1024 AND m.owner_team_id IS NULL AND m.user_id=actor AND m.personal_auto_enabled
 UNION ALL
 SELECT 'automatic',m.id,m.machine_service_policy_generation,b.team_id FROM host m
 JOIN team_resource_bindings b ON b.resource_kind='machine' AND b.resource_id=m.id AND b.active
 WHERE port>=1024 AND m.team_auto_enabled AND team_machine_capability_allowed(actor,b.team_id,m.id,'private_access')
 ;
$$;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'workspace ownership migration is forward-only'; END $$;
-- +goose StatementEnd
