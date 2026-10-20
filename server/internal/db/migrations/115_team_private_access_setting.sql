-- +goose Up
-- One team setting decides whether members may reach the ports of machines shared into the team:
-- "Allow private access to shared machines". It replaces the per-machine team_auto_enabled switch.
-- Off by default; the team's generation moves with it, which fences every active private-access
-- session of the team's members (machine_service_team_fence includes the team generation).
ALTER TABLE paperboat.teams ADD COLUMN allow_private_access boolean NOT NULL DEFAULT false;
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
 WHERE port>=1024 AND (SELECT t.allow_private_access FROM teams t WHERE t.team_id=b.team_id) AND team_machine_capability_allowed(actor,b.team_id,m.id,'private_access')
 ;
$$;
-- +goose StatementEnd
ALTER TABLE paperboat.user_machines DROP COLUMN team_auto_enabled;
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'team private access setting migration is forward-only'; END $$;
-- +goose StatementEnd
