-- +goose Up
-- Canonical v1 terminology preserves installed IDs, identities and generation fences.
ALTER TABLE paperboat.account_network_preferences RENAME COLUMN device_suffix TO machine_suffix;
ALTER TABLE paperboat.account_network_preferences RENAME COLUMN device_loopback_cidr TO machine_loopback_cidr;
ALTER TABLE paperboat.audit_events RENAME COLUMN source_device_id TO source_machine_id;
ALTER TABLE paperboat.preview_lease_carrier_attachments RENAME COLUMN owner_device_id TO owner_machine_id;
ALTER TABLE paperboat.preview_leases RENAME COLUMN owner_device_id TO owner_machine_id;
ALTER TABLE paperboat.user_machines RENAME COLUMN device_service_policy_generation TO machine_service_policy_generation;
ALTER TABLE paperboat.team_network_preferences RENAME COLUMN device_suffix TO machine_suffix;
ALTER TABLE paperboat.team_network_preferences RENAME COLUMN device_loopback_cidr TO machine_loopback_cidr;
ALTER TABLE paperboat.user_machine_access_sessions RENAME COLUMN device_service_port TO machine_service_port;
ALTER TABLE paperboat.user_machine_access_sessions RENAME COLUMN device_service_source_kind TO machine_service_source_kind;
ALTER TABLE paperboat.user_machine_access_sessions RENAME COLUMN device_service_source_id TO machine_service_source_id;
ALTER TABLE paperboat.user_machine_access_sessions RENAME COLUMN device_service_source_generation TO machine_service_source_generation;
ALTER TABLE paperboat.user_machine_access_sessions RENAME COLUMN device_service_team_fence TO machine_service_team_fence;
ALTER TABLE paperboat.device_service_discovery_policies RENAME TO machine_service_discovery_policies;
ALTER TABLE paperboat.device_service_snapshots RENAME TO machine_service_snapshots;
ALTER TABLE paperboat.device_edge_placements RENAME TO machine_edge_placements;
ALTER FUNCTION paperboat.device_service_authorities(text,text,integer) RENAME TO machine_service_authorities;
ALTER FUNCTION paperboat.device_service_session_allowed(text) RENAME TO machine_service_session_allowed;
ALTER FUNCTION paperboat.device_service_team_fence(text,text,text,text,text) RENAME TO machine_service_team_fence;
ALTER FUNCTION paperboat.ensure_device_environment_label() RENAME TO ensure_machine_environment_label;
ALTER TRIGGER device_environment_label ON paperboat.user_machines RENAME TO machine_environment_label;
ALTER TABLE paperboat.account_e2ee_keys DROP CONSTRAINT account_e2ee_keys_revocation_reason_check;
ALTER TABLE paperboat.preview_lease_carrier_attachments DROP CONSTRAINT preview_lease_carrier_attachments_check;
ALTER TABLE paperboat.user_machines DROP CONSTRAINT user_machines_device_service_policy_generation_check;
ALTER TABLE paperboat.lazy_access_policies DROP CONSTRAINT lazy_access_policies_authorization_source_check;
ALTER TABLE paperboat.peer_endpoint_certificate_revocations DROP CONSTRAINT peer_endpoint_certificate_revocations_reason_check;
ALTER TABLE paperboat.peer_endpoint_certificates DROP CONSTRAINT peer_endpoint_certificates_revocation_reason_check;
ALTER TABLE paperboat.user_machine_access_sessions DROP CONSTRAINT machine_access_session_purpose;
ALTER TABLE paperboat.user_machine_access_sessions DROP CONSTRAINT user_machine_access_sessions_device_service_port_check;
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
 SELECT 'automatic',m.id,m.machine_service_policy_generation,NULL::text FROM host m
 WHERE port>=1024 AND m.owner_team_id IS NULL AND m.user_id=actor AND m.personal_auto_enabled
 UNION ALL
 SELECT 'automatic',m.id,m.machine_service_policy_generation,b.team_id FROM host m
 JOIN team_resource_bindings b ON b.resource_kind='machine' AND b.resource_id=m.id AND b.active
 WHERE port>=1024 AND m.team_auto_enabled AND team_machine_capability_allowed(actor,b.team_id,m.id,'private_access')
 ;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.machine_service_session_allowed(session text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT EXISTS(SELECT 1 FROM user_machine_access_sessions a
 JOIN cli_client_sessions c ON c.id=a.cli_client_session_id AND c.user_id=a.user_id AND c.state='active' AND c.revoked_at IS NULL
 CROSS JOIN LATERAL machine_service_authorities(a.user_id,a.user_machine_id,a.machine_service_port) authority
 WHERE a.id=session AND a.state='active' AND a.revoked_at IS NULL AND a.expires_at>now()
 AND authority.source_kind=a.machine_service_source_kind AND authority.source_id=a.machine_service_source_id
 AND authority.source_generation=a.machine_service_source_generation AND authority.team_id IS NOT DISTINCT FROM a.team_id
 AND a.machine_service_team_fence=machine_service_team_fence(a.user_id,a.user_machine_id,a.machine_service_source_kind,a.machine_service_source_id,a.team_id));
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.machine_service_team_fence(actor text, machine text, kind text, source text, team text) RETURNS text
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT CASE WHEN team IS NULL THEN 'personal' ELSE (
 SELECT jsonb_build_array(t.generation,member.membership_generation,b.generation,
 CASE WHEN kind='automatic' THEN (SELECT max(g.generation) FROM team_machine_grants g WHERE g.team_id=t.team_id AND g.machine_id=machine AND g.active AND (g.audience='all_members' OR g.account_id=actor) AND 'private_access'=ANY(g.capabilities))
 ELSE (SELECT g.generation FROM team_resource_grants g WHERE g.team_id=t.team_id AND g.account_id=actor AND g.resource_kind=kind AND g.resource_id=source AND g.active AND g.permission IN ('use','manage')) END)::text
 FROM teams t JOIN team_members member ON member.team_id=t.team_id AND member.account_id=actor AND member.active
 JOIN team_resource_bindings b ON b.team_id=t.team_id AND b.resource_kind=CASE WHEN kind='automatic' THEN 'machine' ELSE kind END AND b.resource_id=CASE WHEN kind='automatic' THEN machine ELSE source END AND b.active
 WHERE t.team_id=team AND t.deleted_at IS NULL
 ) END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.enforce_user_machine_owner_alias() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF TG_OP = 'UPDATE' AND NEW.alias = OLD.alias AND NEW.owner_team_id IS NOT DISTINCT FROM OLD.owner_team_id AND NEW.user_id=OLD.user_id AND NEW.deleted_at IS NOT DISTINCT FROM OLD.deleted_at THEN RETURN NEW; END IF;
 IF NEW.deleted_at IS NOT NULL THEN RETURN NEW; END IF;
 PERFORM pg_advisory_xact_lock(hashtextextended(CASE WHEN NEW.owner_team_id IS NULL THEN NEW.user_id ELSE 'team:'||NEW.owner_team_id END,0));
 IF EXISTS(SELECT 1 FROM user_machines m WHERE m.id<>NEW.id AND m.deleted_at IS NULL AND lower(m.alias)=lower(NEW.alias) AND ((NEW.owner_team_id IS NULL AND m.owner_team_id IS NULL AND m.user_id=NEW.user_id) OR (NEW.owner_team_id IS NOT NULL AND m.owner_team_id=NEW.owner_team_id))) THEN
  RAISE EXCEPTION 'machine alias is already used in this ownership scope' USING ERRCODE='23505', CONSTRAINT='user_machines_owner_alias';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.ensure_machine_environment_label() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
BEGIN
 INSERT INTO lazy_environment_identities(environment_id,environment_label) VALUES(NEW.environment_id,substr(md5(NEW.environment_id||random()::text),1,16)) ON CONFLICT(environment_id) DO NOTHING;
 RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.preview_lease_carrier_attachment_guard_v1() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.state NOT IN ('failed','released') AND NOT EXISTS (
    SELECT 1
    FROM preview_leases AS p
    WHERE p.id = NEW.preview_id
      AND p.account_id = NEW.account_id
      AND p.owner_machine_id = NEW.owner_machine_id
      AND p.owner_session_id = NEW.owner_session_id
      AND p.terminal_state = 'active'
  ) THEN
    RAISE EXCEPTION 'preview attachment owner/session does not match active lease';
  END IF;
  IF NEW.state NOT IN ('failed','released') AND NOT EXISTS (
    SELECT 1
    FROM user_machines AS m
    WHERE m.id = NEW.owner_machine_id
      AND m.user_id = NEW.account_id
      AND m.public_identity_key = NEW.machine_identity_public_key
      AND m.deleted_at IS NULL
      AND m.revoked_at IS NULL
  ) THEN
    RAISE EXCEPTION 'preview attachment machine verifier does not match owner machine';
  END IF;
  IF NEW.state IN ('failed','released') AND NOT EXISTS (
    SELECT 1
    FROM preview_leases AS p
    WHERE p.id = NEW.preview_id
      AND p.account_id = NEW.account_id
      AND p.owner_machine_id = NEW.owner_machine_id
      AND p.owner_session_id = NEW.owner_session_id
  ) THEN
    RAISE EXCEPTION 'preview attachment owner/session does not match lease';
  END IF;
  IF NOT EXISTS (
    SELECT 1
    FROM preview_lease_create_operations AS create_operation
    WHERE create_operation.account_id = NEW.account_id
      AND create_operation.preview_id = NEW.preview_id
      AND create_operation.operation_id = NEW.operation_id
  ) THEN
    RAISE EXCEPTION 'preview attachment operation does not own preview lease';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.preview_management_allowed(actor text, preview text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT EXISTS(SELECT 1 FROM preview_leases p JOIN user_machines m ON m.id=p.owner_machine_id
 WHERE p.id=preview AND (
  (p.account_id=actor AND m.owner_team_id IS NULL AND m.user_id=actor)
  OR (p.access_mode IN ('private','team')
   AND m.configured_capabilities @> ARRAY['preview_launch']::text[]
   AND machine_capability_allowed(actor,m.id,'preview_manage'))
 ));
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.revoke_account_e2ee_key_dependents() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL THEN
    UPDATE peer_endpoint_certificates
    SET revoked_at = NEW.revoked_at,
        revocation_reason = CASE
          WHEN NEW.revocation_reason IN ('machine_removed', 'key_compromise') THEN NEW.revocation_reason
          ELSE 'account_revoked'
        END
    WHERE key_id = NEW.key_id AND revoked_at IS NULL;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.revoke_account_e2ee_keys_for_cli_session() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF OLD.state <> 'revoked' AND NEW.state = 'revoked' THEN
    UPDATE account_e2ee_keys
    SET revoked_at = coalesce(revoked_at, NEW.revoked_at, now()),
        revocation_reason = coalesce(revocation_reason, 'machine_removed'),
        updated_at = coalesce(NEW.revoked_at, now())
    WHERE cli_client_session_id = NEW.id AND revoked_at IS NULL;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.revoke_account_e2ee_keys_for_machine() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL OR
     OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
    UPDATE account_e2ee_keys
    SET revoked_at = coalesce(revoked_at, NEW.revoked_at, NEW.deleted_at, now()),
        revocation_reason = coalesce(revocation_reason, 'machine_removed'),
        updated_at = coalesce(NEW.revoked_at, NEW.deleted_at, now())
    WHERE user_machine_id = NEW.id AND revoked_at IS NULL;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.revoke_cli_peer_endpoint_for_session(session_id text, session_user_id text, session_revoked_at timestamp with time zone, session_revocation_reason text) RETURNS void
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_temp'
    AS $$
DECLARE
  endpoint_reason text := CASE
    WHEN coalesce(session_revocation_reason, '') LIKE 'account_%' THEN 'account_revoked'
    WHEN session_revocation_reason IN ('machine_removed', 'key_compromise') THEN session_revocation_reason
    ELSE 'endpoint_removed'
  END;
  effective_revoked_at timestamptz := coalesce(session_revoked_at, now());
BEGIN
  INSERT INTO peer_endpoint_certificate_revocations
    (operation_id, user_id, certificate_fingerprint, serial, reason, created_at)
  SELECT
    'auth.cli_session_revocation:' || encode(certificate.fingerprint, 'hex'),
    certificate.user_id,
    certificate.fingerprint,
    certificate.serial,
    endpoint_reason,
    effective_revoked_at
  FROM peer_endpoint_certificates certificate
  WHERE certificate.user_id = session_user_id
    AND certificate.endpoint_id = session_id
    AND certificate.role = 'cli'
    AND certificate.revoked_at IS NULL
  ON CONFLICT (user_id, certificate_fingerprint) DO NOTHING;

  UPDATE peer_endpoint_enrollment_requests
  SET state = 'revoked', certificate_fingerprint = NULL, fulfilled_at = NULL
  WHERE user_id = session_user_id
    AND endpoint_id = session_id
    AND role = 'cli'
    AND state IN ('pending', 'fulfilled');

  UPDATE peer_endpoint_certificates
  SET revoked_at = effective_revoked_at,
      revocation_reason = endpoint_reason
  WHERE user_id = session_user_id
    AND endpoint_id = session_id
    AND role = 'cli'
    AND revoked_at IS NULL;
END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.revoke_invalid_team_machine_access() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE affected_machine text; affected_team text;
BEGIN
 IF TG_TABLE_NAME='user_machines' THEN
  affected_machine := NEW.id;
 ELSE
  IF TG_OP='DELETE' THEN affected_team := OLD.team_id; ELSE affected_team := NEW.team_id; END IF;
 END IF;
 UPDATE user_machine_access_sessions a SET state='revoked',revoked_at=now(),updated_at=now(),revocation_reason='machine_permission_revoked'
 WHERE a.state='active' AND ((affected_machine IS NOT NULL AND a.user_machine_id=affected_machine) OR (affected_team IS NOT NULL AND a.team_id=affected_team)) AND (EXISTS(
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
UPDATE paperboat.account_e2ee_keys SET revocation_reason=replace(revocation_reason,'device','machine') WHERE revocation_reason IN ('device_removed','device_service','device_network');
UPDATE paperboat.peer_endpoint_certificate_revocations SET reason=replace(reason,'device','machine') WHERE reason IN ('device_removed','device_service','device_network');
UPDATE paperboat.peer_endpoint_certificates SET revocation_reason=replace(revocation_reason,'device','machine') WHERE revocation_reason IN ('device_removed','device_service','device_network');
UPDATE paperboat.lazy_access_policies SET authorization_source=replace(authorization_source,'device','machine') WHERE authorization_source IN ('device_removed','device_service','device_network');
UPDATE paperboat.cli_client_sessions SET revocation_reason=replace(revocation_reason,'device','machine') WHERE revocation_reason IN ('device_removed','device_service','device_network');
ALTER TABLE paperboat.account_e2ee_keys ADD CONSTRAINT account_e2ee_keys_revocation_reason_check CHECK (((revocation_reason IS NULL) OR (revocation_reason = ANY (ARRAY['machine_removed'::text, 'key_compromise'::text, 'account_revoked'::text]))));
ALTER TABLE paperboat.preview_lease_carrier_attachments ADD CONSTRAINT preview_lease_carrier_attachments_check CHECK ((host_id = owner_machine_id));
ALTER TABLE paperboat.user_machines ADD CONSTRAINT user_machines_machine_service_policy_generation_check CHECK (((machine_service_policy_generation >= 1) AND (machine_service_policy_generation <= '9007199254740991'::bigint)));
ALTER TABLE paperboat.lazy_access_policies ADD CONSTRAINT lazy_access_policies_authorization_source_check CHECK ((authorization_source = ANY (ARRAY['explicit'::text, 'machine_service'::text])));
ALTER TABLE paperboat.peer_endpoint_certificate_revocations ADD CONSTRAINT peer_endpoint_certificate_revocations_reason_check CHECK ((reason = ANY (ARRAY['endpoint_removed'::text, 'machine_removed'::text, 'account_revoked'::text, 'key_compromise'::text])));
ALTER TABLE paperboat.peer_endpoint_certificates ADD CONSTRAINT peer_endpoint_certificates_revocation_reason_check CHECK (((revocation_reason IS NULL) OR (revocation_reason = ANY (ARRAY['endpoint_replaced'::text, 'endpoint_removed'::text, 'machine_removed'::text, 'account_revoked'::text, 'key_compromise'::text, 'certificate_superseded'::text]))));
ALTER TABLE paperboat.user_machine_access_sessions ADD CONSTRAINT machine_access_session_purpose CHECK (((helper_terminal_session_id IS NOT NULL) OR (helper_file_session_id IS NOT NULL) OR ((machine_service_port IS NOT NULL) AND (capabilities = ARRAY['private_access'::text]) AND (machine_service_source_kind = ANY (ARRAY['automatic'::text, 'lazy_policy'::text, 'tunnel'::text])) AND (machine_service_source_id IS NOT NULL) AND (machine_service_source_generation > 0))));
ALTER TABLE paperboat.user_machine_access_sessions ADD CONSTRAINT user_machine_access_sessions_machine_service_port_check CHECK (((machine_service_port >= 1) AND (machine_service_port <= 65535)));
UPDATE paperboat.control_config_assignments
SET path_rules=(SELECT coalesce(jsonb_agg(CASE WHEN rule->>'source'='device' THEN jsonb_set(rule,'{source}','"machine"'::jsonb) ELSE rule END ORDER BY position),'[]'::jsonb) FROM jsonb_array_elements(path_rules) WITH ORDINALITY AS item(rule,position))
WHERE jsonb_path_exists(path_rules,'$[*] ? (@.source == "device")');


-- +goose StatementBegin
DO $$ DECLARE item record; BEGIN
 FOR item IN SELECT conname FROM pg_constraint WHERE conrelid='paperboat.machine_service_discovery_policies'::regclass AND conname LIKE 'device\_service\_discovery\_policies%' ESCAPE '\' LOOP
  EXECUTE format('ALTER TABLE paperboat.%I RENAME CONSTRAINT %I TO %I','machine_service_discovery_policies',item.conname,replace(item.conname,'device_service_discovery_policies','machine_service_discovery_policies'));
 END LOOP;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$ DECLARE item record; BEGIN
 FOR item IN SELECT conname FROM pg_constraint WHERE conrelid='paperboat.machine_service_snapshots'::regclass AND conname LIKE 'device\_service\_snapshots%' ESCAPE '\' LOOP
  EXECUTE format('ALTER TABLE paperboat.%I RENAME CONSTRAINT %I TO %I','machine_service_snapshots',item.conname,replace(item.conname,'device_service_snapshots','machine_service_snapshots'));
 END LOOP;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$ DECLARE item record; BEGIN
 FOR item IN SELECT conname FROM pg_constraint WHERE conrelid='paperboat.machine_edge_placements'::regclass AND conname LIKE 'device\_edge\_placements%' ESCAPE '\' LOOP
  EXECUTE format('ALTER TABLE paperboat.%I RENAME CONSTRAINT %I TO %I','machine_edge_placements',item.conname,replace(item.conname,'device_edge_placements','machine_edge_placements'));
 END LOOP;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$ BEGIN IF to_regclass('paperboat.device_edge_placements_account') IS NOT NULL THEN ALTER INDEX paperboat.device_edge_placements_account RENAME TO machine_edge_placements_account; END IF; END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ DECLARE item record; BEGIN
 FOR item IN SELECT conname FROM pg_constraint WHERE conrelid='paperboat.machine_service_discovery_policies'::regclass AND conname LIKE 'machine\_service\_discovery\_policies%' ESCAPE '\' LOOP
  EXECUTE format('ALTER TABLE paperboat.%I RENAME CONSTRAINT %I TO %I','machine_service_discovery_policies',item.conname,replace(item.conname,'machine_service_discovery_policies','device_service_discovery_policies'));
 END LOOP;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$ DECLARE item record; BEGIN
 FOR item IN SELECT conname FROM pg_constraint WHERE conrelid='paperboat.machine_service_snapshots'::regclass AND conname LIKE 'machine\_service\_snapshots%' ESCAPE '\' LOOP
  EXECUTE format('ALTER TABLE paperboat.%I RENAME CONSTRAINT %I TO %I','machine_service_snapshots',item.conname,replace(item.conname,'machine_service_snapshots','device_service_snapshots'));
 END LOOP;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$ DECLARE item record; BEGIN
 FOR item IN SELECT conname FROM pg_constraint WHERE conrelid='paperboat.machine_edge_placements'::regclass AND conname LIKE 'machine\_edge\_placements%' ESCAPE '\' LOOP
  EXECUTE format('ALTER TABLE paperboat.%I RENAME CONSTRAINT %I TO %I','machine_edge_placements',item.conname,replace(item.conname,'machine_edge_placements','device_edge_placements'));
 END LOOP;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$ BEGIN IF to_regclass('paperboat.machine_edge_placements_account') IS NOT NULL THEN ALTER INDEX paperboat.machine_edge_placements_account RENAME TO device_edge_placements_account; END IF; END $$;
-- +goose StatementEnd

ALTER TABLE paperboat.account_e2ee_keys DROP CONSTRAINT account_e2ee_keys_revocation_reason_check;
ALTER TABLE paperboat.preview_lease_carrier_attachments DROP CONSTRAINT preview_lease_carrier_attachments_check;
ALTER TABLE paperboat.user_machines DROP CONSTRAINT user_machines_machine_service_policy_generation_check;
ALTER TABLE paperboat.lazy_access_policies DROP CONSTRAINT lazy_access_policies_authorization_source_check;
ALTER TABLE paperboat.peer_endpoint_certificate_revocations DROP CONSTRAINT peer_endpoint_certificate_revocations_reason_check;
ALTER TABLE paperboat.peer_endpoint_certificates DROP CONSTRAINT peer_endpoint_certificates_revocation_reason_check;
ALTER TABLE paperboat.user_machine_access_sessions DROP CONSTRAINT machine_access_session_purpose;
ALTER TABLE paperboat.user_machine_access_sessions DROP CONSTRAINT user_machine_access_sessions_machine_service_port_check;
UPDATE paperboat.account_e2ee_keys SET revocation_reason=replace(revocation_reason,'machine','device') WHERE revocation_reason IN ('machine_removed','machine_service','machine_network');
UPDATE paperboat.peer_endpoint_certificate_revocations SET reason=replace(reason,'machine','device') WHERE reason IN ('machine_removed','machine_service','machine_network');
UPDATE paperboat.peer_endpoint_certificates SET revocation_reason=replace(revocation_reason,'machine','device') WHERE revocation_reason IN ('machine_removed','machine_service','machine_network');
UPDATE paperboat.lazy_access_policies SET authorization_source=replace(authorization_source,'machine','device') WHERE authorization_source IN ('machine_removed','machine_service','machine_network');
UPDATE paperboat.cli_client_sessions SET revocation_reason=replace(revocation_reason,'machine','device') WHERE revocation_reason IN ('machine_removed','machine_service','machine_network');
UPDATE paperboat.control_config_assignments
SET path_rules=(SELECT coalesce(jsonb_agg(CASE WHEN rule->>'source'='machine' THEN jsonb_set(rule,'{source}','"device"'::jsonb) ELSE rule END ORDER BY position),'[]'::jsonb) FROM jsonb_array_elements(path_rules) WITH ORDINALITY AS item(rule,position))
WHERE jsonb_path_exists(path_rules,'$[*] ? (@.source == "machine")');
ALTER TABLE paperboat.machine_edge_placements RENAME TO device_edge_placements;
ALTER TABLE paperboat.machine_service_snapshots RENAME TO device_service_snapshots;
ALTER TABLE paperboat.machine_service_discovery_policies RENAME TO device_service_discovery_policies;
ALTER TABLE paperboat.user_machine_access_sessions RENAME COLUMN machine_service_team_fence TO device_service_team_fence;
ALTER TABLE paperboat.user_machine_access_sessions RENAME COLUMN machine_service_source_generation TO device_service_source_generation;
ALTER TABLE paperboat.user_machine_access_sessions RENAME COLUMN machine_service_source_id TO device_service_source_id;
ALTER TABLE paperboat.user_machine_access_sessions RENAME COLUMN machine_service_source_kind TO device_service_source_kind;
ALTER TABLE paperboat.user_machine_access_sessions RENAME COLUMN machine_service_port TO device_service_port;
ALTER TABLE paperboat.team_network_preferences RENAME COLUMN machine_loopback_cidr TO device_loopback_cidr;
ALTER TABLE paperboat.team_network_preferences RENAME COLUMN machine_suffix TO device_suffix;
ALTER TABLE paperboat.user_machines RENAME COLUMN machine_service_policy_generation TO device_service_policy_generation;
ALTER TABLE paperboat.preview_leases RENAME COLUMN owner_machine_id TO owner_device_id;
ALTER TABLE paperboat.preview_lease_carrier_attachments RENAME COLUMN owner_machine_id TO owner_device_id;
ALTER TABLE paperboat.audit_events RENAME COLUMN source_machine_id TO source_device_id;
ALTER TABLE paperboat.account_network_preferences RENAME COLUMN machine_loopback_cidr TO device_loopback_cidr;
ALTER TABLE paperboat.account_network_preferences RENAME COLUMN machine_suffix TO device_suffix;
ALTER FUNCTION paperboat.machine_service_authorities(text,text,integer) RENAME TO device_service_authorities;
ALTER FUNCTION paperboat.machine_service_session_allowed(text) RENAME TO device_service_session_allowed;
ALTER FUNCTION paperboat.machine_service_team_fence(text,text,text,text,text) RENAME TO device_service_team_fence;
ALTER FUNCTION paperboat.ensure_machine_environment_label() RENAME TO ensure_device_environment_label;
ALTER TRIGGER machine_environment_label ON paperboat.user_machines RENAME TO device_environment_label;
ALTER TABLE paperboat.account_e2ee_keys ADD CONSTRAINT account_e2ee_keys_revocation_reason_check CHECK (((revocation_reason IS NULL) OR (revocation_reason = ANY (ARRAY['device_removed'::text, 'key_compromise'::text, 'account_revoked'::text]))));
ALTER TABLE paperboat.preview_lease_carrier_attachments ADD CONSTRAINT preview_lease_carrier_attachments_check CHECK ((host_id = owner_device_id));
ALTER TABLE paperboat.user_machines ADD CONSTRAINT user_machines_device_service_policy_generation_check CHECK (((device_service_policy_generation >= 1) AND (device_service_policy_generation <= '9007199254740991'::bigint)));
ALTER TABLE paperboat.lazy_access_policies ADD CONSTRAINT lazy_access_policies_authorization_source_check CHECK ((authorization_source = ANY (ARRAY['explicit'::text, 'device_service'::text])));
ALTER TABLE paperboat.peer_endpoint_certificate_revocations ADD CONSTRAINT peer_endpoint_certificate_revocations_reason_check CHECK ((reason = ANY (ARRAY['endpoint_removed'::text, 'device_removed'::text, 'account_revoked'::text, 'key_compromise'::text])));
ALTER TABLE paperboat.peer_endpoint_certificates ADD CONSTRAINT peer_endpoint_certificates_revocation_reason_check CHECK (((revocation_reason IS NULL) OR (revocation_reason = ANY (ARRAY['endpoint_replaced'::text, 'endpoint_removed'::text, 'device_removed'::text, 'account_revoked'::text, 'key_compromise'::text, 'certificate_superseded'::text]))));
ALTER TABLE paperboat.user_machine_access_sessions ADD CONSTRAINT machine_access_session_purpose CHECK (((helper_terminal_session_id IS NOT NULL) OR (helper_file_session_id IS NOT NULL) OR ((device_service_port IS NOT NULL) AND (capabilities = ARRAY['private_access'::text]) AND (device_service_source_kind = ANY (ARRAY['automatic'::text, 'lazy_policy'::text, 'tunnel'::text])) AND (device_service_source_id IS NOT NULL) AND (device_service_source_generation > 0))));
ALTER TABLE paperboat.user_machine_access_sessions ADD CONSTRAINT user_machine_access_sessions_device_service_port_check CHECK (((device_service_port >= 1) AND (device_service_port <= 65535)));
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
CREATE OR REPLACE FUNCTION paperboat.device_service_session_allowed(session text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT EXISTS(SELECT 1 FROM user_machine_access_sessions a
 JOIN cli_client_sessions c ON c.id=a.cli_client_session_id AND c.user_id=a.user_id AND c.state='active' AND c.revoked_at IS NULL
 CROSS JOIN LATERAL device_service_authorities(a.user_id,a.user_machine_id,a.device_service_port) authority
 WHERE a.id=session AND a.state='active' AND a.revoked_at IS NULL AND a.expires_at>now()
 AND authority.source_kind=a.device_service_source_kind AND authority.source_id=a.device_service_source_id
 AND authority.source_generation=a.device_service_source_generation AND authority.team_id IS NOT DISTINCT FROM a.team_id
 AND a.device_service_team_fence=device_service_team_fence(a.user_id,a.user_machine_id,a.device_service_source_kind,a.device_service_source_id,a.team_id));
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.device_service_team_fence(actor text, machine text, kind text, source text, team text) RETURNS text
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT CASE WHEN team IS NULL THEN 'personal' ELSE (
 SELECT jsonb_build_array(t.generation,member.membership_generation,b.generation,
 CASE WHEN kind='automatic' THEN (SELECT max(g.generation) FROM team_machine_grants g WHERE g.team_id=t.team_id AND g.machine_id=machine AND g.active AND (g.audience='all_members' OR g.account_id=actor) AND 'private_access'=ANY(g.capabilities))
 ELSE (SELECT g.generation FROM team_resource_grants g WHERE g.team_id=t.team_id AND g.account_id=actor AND g.resource_kind=kind AND g.resource_id=source AND g.active AND g.permission IN ('use','manage')) END)::text
 FROM teams t JOIN team_members member ON member.team_id=t.team_id AND member.account_id=actor AND member.active
 JOIN team_resource_bindings b ON b.team_id=t.team_id AND b.resource_kind=CASE WHEN kind='automatic' THEN 'machine' ELSE kind END AND b.resource_id=CASE WHEN kind='automatic' THEN machine ELSE source END AND b.active
 WHERE t.team_id=team AND t.deleted_at IS NULL
 ) END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.enforce_user_machine_owner_alias() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF TG_OP = 'UPDATE' AND NEW.alias = OLD.alias AND NEW.owner_team_id IS NOT DISTINCT FROM OLD.owner_team_id AND NEW.user_id=OLD.user_id AND NEW.deleted_at IS NOT DISTINCT FROM OLD.deleted_at THEN RETURN NEW; END IF;
 IF NEW.deleted_at IS NOT NULL THEN RETURN NEW; END IF;
 PERFORM pg_advisory_xact_lock(hashtextextended(CASE WHEN NEW.owner_team_id IS NULL THEN NEW.user_id ELSE 'team:'||NEW.owner_team_id END,0));
 IF EXISTS(SELECT 1 FROM user_machines m WHERE m.id<>NEW.id AND m.deleted_at IS NULL AND lower(m.alias)=lower(NEW.alias) AND ((NEW.owner_team_id IS NULL AND m.owner_team_id IS NULL AND m.user_id=NEW.user_id) OR (NEW.owner_team_id IS NOT NULL AND m.owner_team_id=NEW.owner_team_id))) THEN
  RAISE EXCEPTION 'device alias is already used in this ownership scope' USING ERRCODE='23505', CONSTRAINT='user_machines_owner_alias';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.ensure_device_environment_label() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
BEGIN
 INSERT INTO lazy_environment_identities(environment_id,environment_label) VALUES(NEW.environment_id,substr(md5(NEW.environment_id||random()::text),1,16)) ON CONFLICT(environment_id) DO NOTHING;
 RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.preview_lease_carrier_attachment_guard_v1() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.state NOT IN ('failed','released') AND NOT EXISTS (
    SELECT 1
    FROM preview_leases AS p
    WHERE p.id = NEW.preview_id
      AND p.account_id = NEW.account_id
      AND p.owner_device_id = NEW.owner_device_id
      AND p.owner_session_id = NEW.owner_session_id
      AND p.terminal_state = 'active'
  ) THEN
    RAISE EXCEPTION 'preview attachment owner/session does not match active lease';
  END IF;
  IF NEW.state NOT IN ('failed','released') AND NOT EXISTS (
    SELECT 1
    FROM user_machines AS m
    WHERE m.id = NEW.owner_device_id
      AND m.user_id = NEW.account_id
      AND m.public_identity_key = NEW.machine_identity_public_key
      AND m.deleted_at IS NULL
      AND m.revoked_at IS NULL
  ) THEN
    RAISE EXCEPTION 'preview attachment machine verifier does not match owner machine';
  END IF;
  IF NEW.state IN ('failed','released') AND NOT EXISTS (
    SELECT 1
    FROM preview_leases AS p
    WHERE p.id = NEW.preview_id
      AND p.account_id = NEW.account_id
      AND p.owner_device_id = NEW.owner_device_id
      AND p.owner_session_id = NEW.owner_session_id
  ) THEN
    RAISE EXCEPTION 'preview attachment owner/session does not match lease';
  END IF;
  IF NOT EXISTS (
    SELECT 1
    FROM preview_lease_create_operations AS create_operation
    WHERE create_operation.account_id = NEW.account_id
      AND create_operation.preview_id = NEW.preview_id
      AND create_operation.operation_id = NEW.operation_id
  ) THEN
    RAISE EXCEPTION 'preview attachment operation does not own preview lease';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.preview_management_allowed(actor text, preview text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT EXISTS(SELECT 1 FROM preview_leases p JOIN user_machines m ON m.id=p.owner_device_id
 WHERE p.id=preview AND (
  (p.account_id=actor AND m.owner_team_id IS NULL AND m.user_id=actor)
  OR (p.access_mode IN ('private','team')
   AND m.configured_capabilities @> ARRAY['preview_launch']::text[]
   AND machine_capability_allowed(actor,m.id,'preview_manage'))
 ));
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.revoke_account_e2ee_key_dependents() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL THEN
    UPDATE peer_endpoint_certificates
    SET revoked_at = NEW.revoked_at,
        revocation_reason = CASE
          WHEN NEW.revocation_reason IN ('device_removed', 'key_compromise') THEN NEW.revocation_reason
          ELSE 'account_revoked'
        END
    WHERE key_id = NEW.key_id AND revoked_at IS NULL;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.revoke_account_e2ee_keys_for_cli_session() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF OLD.state <> 'revoked' AND NEW.state = 'revoked' THEN
    UPDATE account_e2ee_keys
    SET revoked_at = coalesce(revoked_at, NEW.revoked_at, now()),
        revocation_reason = coalesce(revocation_reason, 'device_removed'),
        updated_at = coalesce(NEW.revoked_at, now())
    WHERE cli_client_session_id = NEW.id AND revoked_at IS NULL;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.revoke_account_e2ee_keys_for_machine() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL OR
     OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
    UPDATE account_e2ee_keys
    SET revoked_at = coalesce(revoked_at, NEW.revoked_at, NEW.deleted_at, now()),
        revocation_reason = coalesce(revocation_reason, 'device_removed'),
        updated_at = coalesce(NEW.revoked_at, NEW.deleted_at, now())
    WHERE user_machine_id = NEW.id AND revoked_at IS NULL;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.revoke_cli_peer_endpoint_for_session(session_id text, session_user_id text, session_revoked_at timestamp with time zone, session_revocation_reason text) RETURNS void
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_temp'
    AS $$
DECLARE
  endpoint_reason text := CASE
    WHEN coalesce(session_revocation_reason, '') LIKE 'account_%' THEN 'account_revoked'
    WHEN session_revocation_reason IN ('device_removed', 'key_compromise') THEN session_revocation_reason
    ELSE 'endpoint_removed'
  END;
  effective_revoked_at timestamptz := coalesce(session_revoked_at, now());
BEGIN
  INSERT INTO peer_endpoint_certificate_revocations
    (operation_id, user_id, certificate_fingerprint, serial, reason, created_at)
  SELECT
    'auth.cli_session_revocation:' || encode(certificate.fingerprint, 'hex'),
    certificate.user_id,
    certificate.fingerprint,
    certificate.serial,
    endpoint_reason,
    effective_revoked_at
  FROM peer_endpoint_certificates certificate
  WHERE certificate.user_id = session_user_id
    AND certificate.endpoint_id = session_id
    AND certificate.role = 'cli'
    AND certificate.revoked_at IS NULL
  ON CONFLICT (user_id, certificate_fingerprint) DO NOTHING;

  UPDATE peer_endpoint_enrollment_requests
  SET state = 'revoked', certificate_fingerprint = NULL, fulfilled_at = NULL
  WHERE user_id = session_user_id
    AND endpoint_id = session_id
    AND role = 'cli'
    AND state IN ('pending', 'fulfilled');

  UPDATE peer_endpoint_certificates
  SET revoked_at = effective_revoked_at,
      revocation_reason = endpoint_reason
  WHERE user_id = session_user_id
    AND endpoint_id = session_id
    AND role = 'cli'
    AND revoked_at IS NULL;
END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.revoke_invalid_team_machine_access() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE affected_machine text; affected_team text;
BEGIN
 IF TG_TABLE_NAME='user_machines' THEN
  affected_machine := NEW.id;
 ELSE
  IF TG_OP='DELETE' THEN affected_team := OLD.team_id; ELSE affected_team := NEW.team_id; END IF;
 END IF;
 UPDATE user_machine_access_sessions a SET state='revoked',revoked_at=now(),updated_at=now(),revocation_reason='machine_permission_revoked'
 WHERE a.state='active' AND ((affected_machine IS NOT NULL AND a.user_machine_id=affected_machine) OR (affected_team IS NOT NULL AND a.team_id=affected_team)) AND (EXISTS(
  SELECT 1 FROM unnest(a.capabilities) capability
  WHERE CASE WHEN a.device_service_port IS NOT NULL THEN NOT device_service_session_allowed(a.id) WHEN a.team_id IS NOT NULL THEN NOT team_machine_capability_allowed(a.user_id,a.team_id,a.user_machine_id,capability)
   ELSE NOT EXISTS(SELECT 1 FROM user_machines m WHERE m.id=a.user_machine_id AND m.user_id=a.user_id AND m.owner_team_id IS NULL AND m.revoked_at IS NULL AND m.deleted_at IS NULL) END)
 OR EXISTS(SELECT 1 FROM user_machines m WHERE m.id=a.user_machine_id AND
  (m.state IN ('revoked','disconnected','deleted')
   OR (a.capabilities && ARRAY['terminal','exec'] AND NOT m.configured_capabilities @> ARRAY['terminal_host'])
   OR ('managed_ssh'=ANY(a.capabilities) AND NOT m.configured_capabilities @> ARRAY['ssh_host'])
   OR ('files'=ANY(a.capabilities) AND NOT m.configured_capabilities @> ARRAY['file_receive']))));
 RETURN NULL;
END $$;
-- +goose StatementEnd
