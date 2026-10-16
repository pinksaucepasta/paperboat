-- +goose Up
-- Admission has already authorized the selected workspace. A personal host
-- shared into a team must not assign that team traffic to its owner.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.snapshot_native_usage_authority() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO paperboat.native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,resource_route_id,machine_name,capabilities,admitted_at,expires_at,retain_until,actor_account_id)
 SELECT NEW.id,m.id,m.installation_generation,
 CASE WHEN pub.id IS NOT NULL THEN coalesce('team:'||pub.owner_team_id,'personal:'||pub.account_id) WHEN t.id IS NOT NULL THEN coalesce('team:'||t.owner_team_id,'personal:'||t.owner_account) ELSE coalesce('team:'||NEW.team_id,'team:'||m.owner_team_id,'personal:'||m.user_id) END,
 CASE WHEN pub.id IS NOT NULL THEN 'tunnel' WHEN NEW.machine_service_source_kind='lazy_policy' THEN 'lazy_policy' WHEN NEW.machine_service_port IS NOT NULL THEN 'machine_service' WHEN t.id IS NOT NULL THEN 'terminal' ELSE 'machine' END,
 coalesce(pub.id,CASE WHEN NEW.machine_service_source_kind='lazy_policy' THEN NEW.machine_service_source_id END,t.id,m.id),
 coalesce(pub.name,CASE WHEN NEW.machine_service_port IS NOT NULL THEN m.alias||':'||NEW.machine_service_port END,t.title,t.name,m.alias),
 CASE WHEN NEW.machine_service_port IS NOT NULL THEN 'tcp:'||NEW.machine_service_port ELSE '' END,m.alias,
 CASE WHEN t.id IS NOT NULL AND cardinality(NEW.capabilities)=0 THEN ARRAY['terminal'] ELSE NEW.capabilities END,NEW.created_at,least(NEW.expires_at,coalesce(NEW.revoked_at,NEW.expires_at)),NEW.expires_at+interval '7 days',NEW.user_id
 FROM paperboat.user_machines m
 LEFT JOIN paperboat.user_machine_terminal_sessions t ON t.id=coalesce(NEW.terminal_session_id,nullif(NEW.operation_id,'')) AND t.user_machine_id=m.id
 LEFT JOIN paperboat.tunnels pub ON NEW.machine_service_source_kind='tunnel' AND pub.id=NEW.machine_service_source_id AND pub.created_by_host_id=m.id
 WHERE m.id=NEW.user_machine_id
 ON CONFLICT(access_session_id) DO UPDATE SET
 actor_account_id=coalesce(native_usage_authorities.actor_account_id,EXCLUDED.actor_account_id),
 expires_at=EXCLUDED.expires_at,retain_until=greatest(native_usage_authorities.retain_until,EXCLUDED.retain_until);
 RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.snapshot_native_usage_authority() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO paperboat.native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,resource_route_id,machine_name,capabilities,admitted_at,expires_at,retain_until,actor_account_id)
 SELECT NEW.id,m.id,m.installation_generation,
 CASE WHEN pub.id IS NOT NULL THEN coalesce('team:'||pub.owner_team_id,'personal:'||pub.account_id) WHEN t.id IS NOT NULL THEN coalesce('team:'||t.owner_team_id,'personal:'||t.owner_account) ELSE coalesce('team:'||m.owner_team_id,'personal:'||m.user_id) END,
 CASE WHEN pub.id IS NOT NULL THEN 'tunnel' WHEN NEW.machine_service_source_kind='lazy_policy' THEN 'lazy_policy' WHEN NEW.machine_service_port IS NOT NULL THEN 'machine_service' WHEN t.id IS NOT NULL THEN 'terminal' ELSE 'machine' END,
 coalesce(pub.id,CASE WHEN NEW.machine_service_source_kind='lazy_policy' THEN NEW.machine_service_source_id END,t.id,m.id),
 coalesce(pub.name,CASE WHEN NEW.machine_service_port IS NOT NULL THEN m.alias||':'||NEW.machine_service_port END,t.title,t.name,m.alias),
 CASE WHEN NEW.machine_service_port IS NOT NULL THEN 'tcp:'||NEW.machine_service_port ELSE '' END,m.alias,
 CASE WHEN t.id IS NOT NULL AND cardinality(NEW.capabilities)=0 THEN ARRAY['terminal'] ELSE NEW.capabilities END,NEW.created_at,least(NEW.expires_at,coalesce(NEW.revoked_at,NEW.expires_at)),NEW.expires_at+interval '7 days',NEW.user_id
 FROM paperboat.user_machines m
 LEFT JOIN paperboat.user_machine_terminal_sessions t ON t.id=coalesce(NEW.terminal_session_id,nullif(NEW.operation_id,'')) AND t.user_machine_id=m.id
 LEFT JOIN paperboat.tunnels pub ON NEW.machine_service_source_kind='tunnel' AND pub.id=NEW.machine_service_source_id AND pub.created_by_host_id=m.id
 WHERE m.id=NEW.user_machine_id
 ON CONFLICT(access_session_id) DO UPDATE SET
 actor_account_id=coalesce(native_usage_authorities.actor_account_id,EXCLUDED.actor_account_id),
 expires_at=EXCLUDED.expires_at,retain_until=greatest(native_usage_authorities.retain_until,EXCLUDED.retain_until);
 RETURN NEW;
END $$;
-- +goose StatementEnd
