-- +goose Up
-- Unified devices retain their identities and explicit selections. The original
-- restricted default (revision 1) expands once; explicit revisions remain intact.
UPDATE paperboat.user_machines
SET configured_capabilities = ARRAY['file_receive','preview_launch','terminal_host','session_host','ssh_host','keep_awake','environment_injection']::text[],
    capabilities_desired_version = capabilities_desired_version + 1,
    capabilities_status = 'pending', capabilities_error_code = NULL,
    updated_at = now(), version = version + 1
WHERE setup_mode = 'client' AND capabilities_desired_version = 1
  AND configured_capabilities <@ ARRAY['file_receive','preview_launch']::text[]
  AND configured_capabilities @> ARRAY['file_receive','preview_launch']::text[];
UPDATE paperboat.user_machines SET seat_state='occupied',updated_at=now(),version=version+1
WHERE setup_mode='client' AND seat_state='released' AND revoked_at IS NULL AND deleted_at IS NULL
 AND state NOT IN ('pending','revoked','deleted');

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.device_service_authorities(actor text, machine text, port integer) RETURNS TABLE(source_kind text, source_id text, source_generation bigint, team_id text)
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 WITH host AS (
 SELECT m.* FROM user_machines m JOIN users owner ON owner.id=m.user_id AND owner.status='active'
 WHERE m.id=machine AND port BETWEEN 1 AND 65535 AND m.revoked_at IS NULL AND m.deleted_at IS NULL
 AND m.state NOT IN ('pending','revoked','deleted') AND m.seat_state='occupied'
 AND EXISTS(SELECT 1 FROM users u WHERE u.id=actor AND u.status='active')
 ), explicit AS (
 SELECT 'lazy_policy'::text kind,p.id,p.generation,p.account_id,p.access_mode FROM host m JOIN lazy_access_policies p ON p.machine_id=m.id
 WHERE p.authorization_source='explicit' AND p.installation_generation=m.installation_generation AND p.deleted_at IS NULL AND p.expires_at>now()
 AND p.target_address IN ('127.0.0.1:'||port,'[::1]:'||port)
 UNION ALL
 SELECT 'tunnel',t.id,r.generation,t.account_id,t.access_mode FROM host m JOIN tunnels t ON t.created_by_host_id=m.id JOIN tunnel_routes r ON r.tunnel_id=t.id
 WHERE t.account_id=m.user_id AND t.desired_state='active' AND t.deleted_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>now())
 AND r.protocol='private_tcp' AND r.desired_state='active' AND r.deleted_at IS NULL AND r.origin_address IN ('127.0.0.1:'||port,'[::1]:'||port)
 )
 SELECT e.kind,e.id,e.generation,NULL::text FROM explicit e CROSS JOIN host m
 WHERE m.owner_team_id IS NULL AND m.user_id=actor AND e.account_id=m.user_id AND e.access_mode='private'
 UNION ALL
 SELECT e.kind,e.id,e.generation,b.team_id FROM explicit e CROSS JOIN host m
 JOIN team_resource_bindings b ON b.resource_kind=e.kind AND b.resource_id=e.id AND b.owner_account=m.user_id AND b.active
 JOIN teams t ON t.team_id=b.team_id AND t.deleted_at IS NULL
 JOIN team_members member ON member.team_id=b.team_id AND member.account_id=actor AND member.active
 JOIN team_resource_grants g ON g.team_id=b.team_id AND g.account_id=actor AND g.resource_kind=e.kind AND g.resource_id=e.id AND g.active AND g.permission IN ('use','manage')
 WHERE team_paid_entitled(b.team_id) AND ((m.owner_team_id IS NULL AND e.access_mode='team') OR m.owner_team_id=b.team_id)
 UNION ALL
 SELECT 'automatic',m.id,m.device_service_policy_generation,NULL::text FROM host m
 WHERE port>=1024 AND m.owner_team_id IS NULL AND m.user_id=actor AND m.personal_auto_enabled
 UNION ALL
 SELECT 'automatic',m.id,m.device_service_policy_generation,b.team_id FROM host m
 JOIN team_resource_bindings b ON b.resource_kind='machine' AND b.resource_id=m.id AND b.active
 WHERE port>=1024 AND m.team_auto_enabled AND team_machine_capability_allowed(actor,b.team_id,m.id,'private_access')
 ;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.runtime_carrier_current(wanted_route text, require_ready boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
 SELECT EXISTS (
 SELECT 1 FROM runtime_carrier_attachments a
 JOIN user_machines m ON m.id=a.machine_id
 JOIN control_routes r ON r.id=a.route_id
 JOIN control_environments e ON e.id=r.environment_id
 JOIN control_tunnel_nodes n ON n.id=a.edge_node_id
 JOIN users u ON u.id=a.account_id
 WHERE a.route_id=wanted_route AND a.expires_at>now()
 AND m.user_id=a.account_id AND u.status='active'
 AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.seat_state='occupied'
  AND m.installation_generation=a.installation_generation AND m.worker_generation=a.worker_generation
 AND m.public_identity_key=a.public_identity_key
 AND r.environment_id=m.environment_id AND r.desired_revision=a.route_revision
 AND r.kind='runtime_https_wss' AND r.desired_state IN ('attached','replacing') AND e.desired_state='active'
 AND n.ready AND n.state='ready' AND n.process_epoch=a.edge_process_epoch
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.last_heartbeat_at>now()-interval '30 seconds' AND n.registry_expires_at>now()
 AND (n.drain_deadline IS NULL OR n.drain_deadline>now())
 AND paperboat.node_pool_eligible(n.id,a.account_id,'tunnel')
 AND (NOT require_ready OR (a.ready AND a.observed_at>now()-interval '30 seconds'))
 );
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.runtime_carrier_valid(wanted_route text,wanted_node text,wanted_epoch text,require_ready boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
 SELECT EXISTS (
 SELECT 1 FROM runtime_carrier_attachments a
 JOIN user_machines m ON m.id=a.machine_id
 JOIN control_routes r ON r.id=a.route_id
 JOIN control_environments e ON e.id=r.environment_id
 JOIN control_tunnel_nodes n ON n.id=a.edge_node_id
 JOIN users u ON u.id=a.account_id
 WHERE a.route_id=wanted_route AND a.edge_node_id=wanted_node AND a.edge_process_epoch=wanted_epoch
 AND a.expires_at>now() AND m.user_id=a.account_id AND u.status='active'
 AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.seat_state='occupied'
 AND m.installation_generation=a.installation_generation AND m.worker_generation=a.worker_generation
 AND m.public_identity_key=a.public_identity_key
 AND r.environment_id=m.environment_id AND r.desired_revision=a.route_revision
 AND r.kind='runtime_https_wss' AND r.desired_state IN ('attached','replacing') AND e.desired_state='active'
 AND n.ready AND n.state='ready' AND n.process_epoch=a.edge_process_epoch
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.last_heartbeat_at>now()-interval '30 seconds' AND n.registry_expires_at>now()
 AND (n.drain_deadline IS NULL OR n.drain_deadline>now())
 AND NOT EXISTS (SELECT 1 FROM selfhost_installations i WHERE i.node_id=n.id)
 AND paperboat.node_pool_eligible(n.id,a.account_id,'tunnel')
 AND (NOT require_ready OR (a.ready AND a.observed_at>now()-interval '30 seconds'))
 );
$$;
-- +goose StatementEnd

-- Pending installation grants were issued for the retired wire contract.
-- Deny those bootstrap payloads at cutover; enrolled helper identities remain active.
WITH expired AS (
 UPDATE paperboat.user_machine_pairings
 SET state='expired',expires_at=least(expires_at,now()),updated_at=now(),installation_config_ciphertext=NULL
 WHERE installation_config_ciphertext IS NOT NULL AND state IN ('approved','consumed')
 RETURNING id,approved_by_user_id,authenticated_setup_helper_enrollment_id
)
UPDATE paperboat.control_helper_enrollments e
SET state='revoked',revoked_at=coalesce(e.revoked_at,now())
FROM expired p WHERE (e.id=p.authenticated_setup_helper_enrollment_id OR e.operation_key='helper-enrollment:'||p.approved_by_user_id||':machine-enrollment:'||p.id) AND e.state='pending' AND e.revoked_at IS NULL;
UPDATE paperboat.user_machine_enrollments
SET state='failed_retryable',updated_at=now()
WHERE state IN ('approved','material_issued','installing','connecting') AND pairing_id IN (SELECT id FROM paperboat.user_machine_pairings WHERE state='expired' AND installation_config_ciphertext IS NULL);

ALTER TABLE paperboat.user_machine_pairings DROP CONSTRAINT user_machine_pairings_authenticated_setup_check;
ALTER TABLE paperboat.user_machine_pairings DROP COLUMN authenticated_setup_mode;
ALTER TABLE paperboat.user_machine_pairings ADD CONSTRAINT user_machine_pairings_authenticated_setup_check CHECK (
 (authenticated_setup_cli_session_id IS NULL AND authenticated_setup_operation_id IS NULL AND authenticated_setup_generation IS NULL AND authenticated_setup_helper_enrollment_id IS NULL)
 OR (authenticated_setup_cli_session_id IS NOT NULL AND authenticated_setup_operation_id IS NOT NULL
 AND length(authenticated_setup_operation_id) BETWEEN 8 AND 128
 AND authenticated_setup_generation IS NOT NULL AND authenticated_setup_generation >= 1
 AND approved_by_user_id IS NOT NULL AND user_machine_id IS NOT NULL AND state IN ('approved','consumed','expired')));
ALTER TABLE paperboat.user_machines DROP COLUMN setup_mode, DROP COLUMN setup_roles;
ALTER TABLE paperboat.user_machines ALTER COLUMN configured_capabilities SET DEFAULT ARRAY['file_receive','preview_launch','terminal_host','session_host','ssh_host','keep_awake','environment_injection']::text[];
DROP TABLE paperboat.polar_events;

-- +goose Down
-- Role removal and identity-preserving capability migration are intentionally irreversible.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'unified device migration cannot restore retired roles'; END $$;
-- +goose StatementEnd
