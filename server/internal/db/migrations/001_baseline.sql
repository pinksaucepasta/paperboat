-- +goose Up
-- +goose StatementBegin
--
-- PostgreSQL database dump
--


-- Dumped from database version 17.10
-- Dumped by pg_dump version 17.10

SET LOCAL statement_timeout = 0;
SET LOCAL lock_timeout = 0;
SET LOCAL idle_in_transaction_session_timeout = 0;
SET LOCAL transaction_timeout = 0;
SET LOCAL client_encoding = 'UTF8';
SET LOCAL standard_conforming_strings = on;
SET LOCAL check_function_bodies = false;
SET LOCAL xmloption = content;
SET LOCAL client_min_messages = warning;
SET LOCAL row_security = off;

--
-- Name: paperboat; Type: SCHEMA; Schema: -; Owner: -
--

CREATE SCHEMA IF NOT EXISTS paperboat;



--
-- Name: advance_peer_network_for_access_session(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.advance_peer_network_for_access_session() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 UPDATE peer_network_identities n SET
  relay_revocation_generation=CASE WHEN TG_OP='INSERT' THEN relay_revocation_generation ELSE config_generation+1 END,
  config_generation=config_generation+1,updated_at=now()
 WHERE (n.user_id=NEW.user_id AND n.endpoint_id=NEW.cli_client_session_id)
 OR (n.endpoint_id=NEW.user_machine_id AND n.user_id=(SELECT user_id FROM user_machines WHERE id=NEW.user_machine_id));
 RETURN NEW;
END $$;


--
-- Name: advance_peer_network_for_account(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.advance_peer_network_for_account() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  UPDATE peer_network_identities
  SET relay_revocation_generation = config_generation + 1, config_generation = config_generation + 1, updated_at = now()
  WHERE user_id = NEW.id;
  RETURN NEW;
END $$;


--
-- Name: advance_peer_network_for_account_key(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.advance_peer_network_for_account_key() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id;
  RETURN NEW;
END $$;


--
-- Name: advance_peer_network_for_account_root(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.advance_peer_network_for_account_root() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  UPDATE peer_network_identities
  SET relay_revocation_generation = config_generation + 1, config_generation = config_generation + 1, updated_at = now()
  WHERE user_id = NEW.user_id;
  RETURN NEW;
END $$;


--
-- Name: advance_peer_network_for_certificate(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.advance_peer_network_for_certificate() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND (n.endpoint_id = NEW.endpoint_id OR EXISTS (
    SELECT 1 FROM user_machine_access_sessions a
    WHERE a.user_id = NEW.user_id AND
      ((a.cli_client_session_id = NEW.endpoint_id AND a.user_machine_id = n.endpoint_id) OR
       (a.user_machine_id = NEW.endpoint_id AND a.cli_client_session_id = n.endpoint_id))));
  RETURN NEW;
END $$;


--
-- Name: advance_peer_network_for_cli_session(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.advance_peer_network_for_cli_session() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND (n.endpoint_id = NEW.id OR EXISTS (
    SELECT 1 FROM user_machine_access_sessions a
    WHERE a.user_id = NEW.user_id AND a.cli_client_session_id = NEW.id
      AND a.user_machine_id = n.endpoint_id));
  RETURN NEW;
END $$;


--
-- Name: advance_peer_network_for_environment(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.advance_peer_network_for_environment() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.owner_user_id AND EXISTS (
    SELECT 1 FROM user_machine_access_sessions a
    WHERE a.user_id = NEW.owner_user_id AND a.environment_id = NEW.id
      AND (a.cli_client_session_id = n.endpoint_id OR a.user_machine_id = n.endpoint_id));
  RETURN NEW;
END $$;


--
-- Name: advance_peer_network_for_identity(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.advance_peer_network_for_identity() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND n.endpoint_id <> NEW.endpoint_id AND EXISTS (
    SELECT 1 FROM user_machine_access_sessions a
    WHERE a.user_id = NEW.user_id AND
      ((a.cli_client_session_id = NEW.endpoint_id AND a.user_machine_id = n.endpoint_id) OR
       (a.user_machine_id = NEW.endpoint_id AND a.cli_client_session_id = n.endpoint_id)));
  RETURN NEW;
END $$;


--
-- Name: advance_peer_network_for_machine(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.advance_peer_network_for_machine() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  UPDATE peer_network_identities n
  SET relay_revocation_generation = n.config_generation + 1, config_generation = n.config_generation + 1, updated_at = now()
  WHERE n.user_id = NEW.user_id AND (n.endpoint_id = NEW.id OR EXISTS (
    SELECT 1 FROM user_machine_access_sessions a
    WHERE a.user_id = NEW.user_id AND a.user_machine_id = NEW.id
      AND a.cli_client_session_id = n.endpoint_id));
  RETURN NEW;
END $$;


--
-- Name: assign_user_machine_fallback_alias(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.assign_user_machine_fallback_alias() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.alias IS NULL THEN
    NEW.alias := 'machine-' || substr(md5(NEW.id), 1, 12);
  END IF;
  RETURN NEW;
END;
$$;


--
-- Name: check_shared_443_names_v1(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.check_shared_443_names_v1() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM shared_443_names_v1 a JOIN shared_443_names_v1 b ON a.passthrough AND NOT b.passthrough
    WHERE a.hostname=b.hostname OR
      (b.hostname LIKE '*.%' AND substring(a.hostname FROM position('.' IN a.hostname)+1)=substring(b.hostname FROM 3))
  ) THEN
    RAISE EXCEPTION 'hostname is already published in the other TLS mode on port 443; delete the conflicting route or domain, or choose another hostname'
      USING ERRCODE='23505',CONSTRAINT='shared_443_hostname_mode_conflict';
  END IF;
  RETURN NULL;
END $$;


--
-- Name: control_account_config_sync_revocation(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.control_account_config_sync_revocation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_temp'
    AS $$
DECLARE
  environment record;
BEGIN
  IF OLD.status = 'active' AND NEW.status <> 'active' THEN
    FOR environment IN
      SELECT id
      FROM control_environments
      WHERE owner_user_id = NEW.id
    LOOP
      PERFORM revoke_config_sync_for_environment(environment.id);
    END LOOP;
  END IF;
  RETURN NEW;
END;
$$;


--
-- Name: control_environment_config_sync_revocation(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.control_environment_config_sync_revocation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_temp'
    AS $$
BEGIN
  IF OLD.desired_state = 'active' AND NEW.desired_state <> 'active' THEN
    PERFORM revoke_config_sync_for_environment(NEW.id);
  END IF;
  RETURN NEW;
END;
$$;


--
-- Name: control_helper_config_sync_revocation(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.control_helper_config_sync_revocation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_temp'
    AS $$
BEGIN
  IF OLD.state = 'active' AND NEW.state <> 'active' THEN
    PERFORM revoke_config_sync_for_environment(NEW.environment_id);
  END IF;
  RETURN NEW;
END;
$$;


--
-- Name: control_repository_config_sync_revocation(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.control_repository_config_sync_revocation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_temp'
    AS $$
DECLARE
  environment record;
BEGIN
  IF OLD.state = 'active' AND NEW.state <> 'active' THEN
    FOR environment IN
      SELECT assignment.environment_id
      FROM control_config_assignments assignment
      WHERE assignment.repository_id = NEW.id
    LOOP
      PERFORM revoke_config_sync_for_environment(environment.environment_id);
    END LOOP;
  END IF;
  RETURN NEW;
END;
$$;


--
-- Name: device_service_authorities(text, text, integer); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.device_service_authorities(actor text, machine text, port integer) RETURNS TABLE(source_kind text, source_id text, source_generation bigint, team_id text)
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 WITH host AS (
 SELECT m.* FROM user_machines m JOIN users owner ON owner.id=m.user_id AND owner.status='active'
 WHERE m.id=machine AND port BETWEEN 1 AND 65535 AND m.revoked_at IS NULL AND m.deleted_at IS NULL
 AND m.state NOT IN ('pending','revoked','deleted') AND (m.setup_mode='client' OR m.seat_state='occupied')
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


--
-- Name: device_service_session_allowed(text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.device_service_session_allowed(session text) RETURNS boolean
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


--
-- Name: device_service_team_fence(text, text, text, text, text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.device_service_team_fence(actor text, machine text, kind text, source text, team text) RETURNS text
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


--
-- Name: end_terminal_sharing_with_session(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.end_terminal_sharing_with_session() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE session text;
BEGIN
 IF TG_OP='DELETE' THEN session:=OLD.id; ELSE session:=NEW.id; END IF;
 IF TG_OP='DELETE' THEN
  UPDATE team_resource_bindings SET active=false,generation=generation+1 WHERE resource_kind='terminal_session' AND resource_id=session AND active;
 ELSIF NEW.deleted_at IS NOT NULL OR NEW.desired_state IN ('closed','deleted') OR NEW.runtime_state IN ('closed','exited','deleted') THEN
  UPDATE team_resource_bindings SET active=false,generation=generation+1 WHERE resource_kind='terminal_session' AND resource_id=session AND active;
 END IF;
 RETURN NULL;
END $$;


--
-- Name: enforce_preview_tunnel_domain_hostname_claim_v1(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.enforce_preview_tunnel_domain_hostname_claim_v1() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
  candidate text := lower(trim(NEW.hostname));
  conflict_exists boolean;
BEGIN
  IF NEW.deleted_at IS NOT NULL THEN
    RETURN NEW;
  END IF;

  PERFORM pg_advisory_xact_lock(hashtextextended(candidate, 145));

  IF TG_TABLE_NAME = 'preview_domains' THEN
    SELECT EXISTS (
      SELECT 1
      FROM tunnel_domains AS other
      WHERE other.hostname = candidate
        AND (
          other.deleted_at IS NULL
          OR (other.account_id <> NEW.account_id AND other.quarantine_until > now())
        )
      UNION ALL
      SELECT 1
      FROM preview_domains AS other
      WHERE other.id <> NEW.id
        AND other.hostname = candidate
        AND (
          other.deleted_at IS NULL
          OR (other.account_id <> NEW.account_id AND other.quarantine_until > now())
        )
      LIMIT 1
    ) INTO conflict_exists;
  ELSE
    SELECT EXISTS (
      SELECT 1
      FROM preview_domains AS other
      WHERE other.hostname = candidate
        AND (
          other.deleted_at IS NULL
          OR (other.account_id <> NEW.account_id AND other.quarantine_until > now())
        )
      UNION ALL
      SELECT 1
      FROM tunnel_domains AS other
      WHERE other.id <> NEW.id
        AND other.hostname = candidate
        AND (
          other.deleted_at IS NULL
          OR (other.account_id <> NEW.account_id AND other.quarantine_until > now())
        )
      LIMIT 1
    ) INTO conflict_exists;
  END IF;

  IF conflict_exists THEN
    RAISE unique_violation USING
      CONSTRAINT = 'domain_bindings_live_hostname_unique',
      MESSAGE = 'custom domain hostname is already claimed';
  END IF;
  RETURN NEW;
END
$$;


--
-- Name: enforce_user_machine_owner_alias(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.enforce_user_machine_owner_alias() RETURNS trigger
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


--
-- Name: ensure_control_sync_lease(text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.ensure_control_sync_lease(requested_machine text) RETURNS TABLE(address_slot integer, tags jsonb)
    LANGUAGE plpgsql
    AS $$
DECLARE candidate integer;
BEGIN
 RETURN QUERY SELECT l.address_slot,l.tags FROM paperboat.control_sync_leases l WHERE l.machine_id=requested_machine;
 IF FOUND THEN RETURN; END IF;
 -- Serialize only allocation, never hold this lock for the stream lifetime.
 LOCK TABLE paperboat.control_sync_leases IN SHARE ROW EXCLUSIVE MODE;
 RETURN QUERY SELECT l.address_slot,l.tags FROM paperboat.control_sync_leases l WHERE l.machine_id=requested_machine;
 IF FOUND THEN RETURN; END IF;
 SELECT n INTO candidate FROM generate_series(2,65534) AS n
 WHERE n % 256 BETWEEN 1 AND 254 AND NOT EXISTS(SELECT 1 FROM paperboat.control_sync_leases l WHERE l.address_slot=n)
 ORDER BY n LIMIT 1;
 IF candidate IS NULL THEN RAISE EXCEPTION 'control sync address pool exhausted' USING ERRCODE='54000'; END IF;
 RETURN QUERY INSERT INTO paperboat.control_sync_leases(machine_id,address_slot) VALUES(requested_machine,candidate)
 RETURNING control_sync_leases.address_slot,control_sync_leases.tags;
END;
$$;


--
-- Name: ensure_device_environment_label(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.ensure_device_environment_label() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
BEGIN
 INSERT INTO lazy_environment_identities(environment_id,environment_label) VALUES(NEW.environment_id,substr(md5(NEW.environment_id||random()::text),1,16)) ON CONFLICT(environment_id) DO NOTHING;
 RETURN NEW;
END $$;


--
-- Name: expire_environment_personal_rotation(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.expire_environment_personal_rotation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 DELETE FROM environment_vault_personal_rotations WHERE account_id=NEW.account_id AND expected_vault_document_id<>NEW.document_id;
 RETURN NEW;
END;
$$;


--
-- Name: expire_team_paid_entitlements(integer); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.expire_team_paid_entitlements(batch_limit integer DEFAULT 256) RETURNS integer
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
DECLARE changed integer;
BEGIN
 IF batch_limit<1 OR batch_limit>1000 THEN
  RAISE EXCEPTION 'batch limit must be between 1 and 1000';
 END IF;
 WITH expired AS (
  SELECT id FROM billing_workspaces
  WHERE team_id IS NOT NULL AND state IN ('active','trialing') AND period_end<=now()
  ORDER BY period_end,id LIMIT batch_limit FOR UPDATE SKIP LOCKED
 ), canceled AS (
  UPDATE billing_workspaces w SET state='canceled',revision=revision+1,updated_at=now()
  FROM expired e WHERE w.id=e.id
  RETURNING w.team_id
 ), fenced AS (
  UPDATE teams t SET generation=t.generation+1
  WHERE t.team_id IN (SELECT DISTINCT team_id FROM canceled)
  RETURNING t.team_id
 )
 SELECT count(*) INTO changed FROM canceled;
 RETURN changed;
END $$;


--
-- Name: fence_peer_network_local_identity(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.fence_peer_network_local_identity() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF ROW(NEW.wireguard_public_key,NEW.disco_public_key,NEW.quic_certificate_fingerprint,NEW.key_generation,NEW.revoked_at,NEW.endpoint_generation,NEW.machine_generation)
 IS DISTINCT FROM ROW(OLD.wireguard_public_key,OLD.disco_public_key,OLD.quic_certificate_fingerprint,OLD.key_generation,OLD.revoked_at,OLD.endpoint_generation,OLD.machine_generation) THEN
  NEW.config_generation := GREATEST(NEW.config_generation, OLD.config_generation + 1);
  NEW.relay_revocation_generation := NEW.config_generation;
 END IF;
 RETURN NEW;
END $$;


--
-- Name: fence_team_paid_entitlement_change(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.fence_team_paid_entitlement_change() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
DECLARE
 team text;
 old_paid boolean := false;
 new_paid boolean := false;
BEGIN
 IF TG_OP<>'INSERT' THEN
  team:=OLD.team_id;
  old_paid:=OLD.team_id IS NOT NULL AND OLD.state IN ('active','trialing') AND OLD.period_start IS NOT NULL AND OLD.period_start<=now() AND OLD.period_end IS NOT NULL AND now()<OLD.period_end;
 END IF;
 IF TG_OP<>'DELETE' THEN
  team:=coalesce(NEW.team_id,team);
  new_paid:=NEW.team_id IS NOT NULL AND NEW.state IN ('active','trialing') AND NEW.period_start IS NOT NULL AND NEW.period_start<=now() AND NEW.period_end IS NOT NULL AND now()<NEW.period_end;
 END IF;
 IF team IS NOT NULL AND old_paid IS DISTINCT FROM new_paid THEN
  UPDATE teams SET generation=generation+1 WHERE team_id=team;
 END IF;
 RETURN NULL;
END $$;


--
-- Name: lock_shared_443_names_v1(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.lock_shared_443_names_v1() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(1885686843);
  RETURN NULL;
END $$;


--
-- Name: machine_capability_allowed(text, text, text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.machine_capability_allowed(actor text, machine text, capability text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT capability=ANY(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage','private_access']) AND (
  EXISTS(SELECT 1 FROM user_machines WHERE id=machine AND user_id=actor AND owner_team_id IS NULL AND revoked_at IS NULL AND deleted_at IS NULL)
  OR EXISTS(SELECT 1 FROM team_resource_bindings b WHERE b.resource_kind='machine' AND b.resource_id=machine AND team_machine_capability_allowed(actor,b.team_id,machine,capability))
 );
$$;


--
-- Name: machine_management_allowed(text, text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.machine_management_allowed(actor text, machine text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT EXISTS(SELECT 1 FROM user_machines m WHERE m.id=machine AND m.deleted_at IS NULL AND (
  (m.owner_team_id IS NULL AND m.user_id=actor) OR EXISTS(SELECT 1 FROM teams t JOIN team_members member USING(team_id) WHERE t.team_id=m.owner_team_id AND t.deleted_at IS NULL AND member.account_id=actor AND member.active AND (t.owner_account=actor OR member.role='admin'))
 ));
$$;


--
-- Name: node_pool_eligible(text, text, text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.node_pool_eligible(wanted_node text, wanted_account text, wanted_capability text) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
 SELECT wanted_capability IN ('relay','tunnel') AND EXISTS (
 SELECT 1 FROM control_tunnel_nodes n
 LEFT JOIN selfhost_installations i ON i.node_id=n.id
 LEFT JOIN managed_node_authority m ON m.node_id=n.id
 LEFT JOIN operator_regions r ON r.id=m.region_id
 LEFT JOIN selfhost_pool_policies p ON p.account_id=wanted_account AND p.capability=wanted_capability
 WHERE n.id=wanted_node AND (
   (i.id IS NULL AND (m.node_id IS NULL OR (m.retired_at IS NULL AND r.enabled)) AND coalesce(p.mode,'mixed')='mixed' AND (n.allowed_account_ids IS NULL OR wanted_account=ANY(n.allowed_account_ids)))
   OR (i.state='active' AND (i.expires_at IS NULL OR i.expires_at>now()) AND i.capability=wanted_capability
     AND EXISTS(SELECT 1 FROM selfhost_pairings g WHERE g.installation_id=i.id AND g.account_id=wanted_account AND g.capability=wanted_capability AND g.revoked_at IS NULL)
     AND (p.account_id IS NULL OR i.id=ANY(p.installation_ids)))
 ))
$$;


--
-- Name: node_preview_eligible(text, text, text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.node_preview_eligible(wanted_node text, wanted_account text, wanted_preview text) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
  SELECT paperboat.node_pool_eligible(wanted_node,wanted_account,'tunnel')
     AND (NOT EXISTS (SELECT 1 FROM paperboat.selfhost_installations i WHERE i.node_id=wanted_node AND i.state='active')
       OR EXISTS (SELECT 1 FROM paperboat.preview_domains d WHERE d.preview_id=wanted_preview AND d.account_id=wanted_account AND d.deleted_at IS NULL AND d.ownership_state='verified' AND d.conflict_state='clear'));
$$;


--
-- Name: node_route_eligible(text, text, text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.node_route_eligible(wanted_node text, wanted_account text, wanted_route text) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
  SELECT paperboat.node_pool_eligible(wanted_node,wanted_account,'tunnel')
     AND (NOT EXISTS (SELECT 1 FROM paperboat.selfhost_installations i WHERE i.node_id=wanted_node AND i.state='active')
       OR EXISTS (SELECT 1 FROM paperboat.tunnel_routes r WHERE r.id=wanted_route AND r.protocol <> 'http')
       OR EXISTS (SELECT 1 FROM paperboat.tunnel_domains d WHERE d.route_id=wanted_route AND d.account_id=wanted_account AND d.deleted_at IS NULL AND d.ownership_state='verified' AND d.conflict_state='clear'));
$$;


--
-- Name: preview_lease_carrier_attachment_edge_epoch_guard_v1(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.preview_lease_carrier_attachment_edge_epoch_guard_v1() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.state NOT IN ('failed','released') AND NOT EXISTS (
    SELECT 1
    FROM control_tunnel_nodes AS node
    WHERE node.id = NEW.edge_node_id
      AND node.process_epoch = NEW.edge_process_epoch
  ) THEN
    RAISE EXCEPTION 'preview attachment edge process epoch is stale';
  END IF;
  RETURN NEW;
END;
$$;


--
-- Name: preview_lease_carrier_attachment_guard_v1(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.preview_lease_carrier_attachment_guard_v1() RETURNS trigger
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


--
-- Name: preview_lease_create_operation_relation_v1(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.preview_lease_create_operation_relation_v1() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.operation_type = 'preview.create'
     AND NEW.resource_kind = 'preview_lease'
     AND NEW.resource_id IS NOT NULL THEN
    INSERT INTO preview_lease_create_operations (account_id, preview_id, operation_id)
    SELECT NEW.account_id, NEW.resource_id, NEW.id
    WHERE EXISTS (
      SELECT 1 FROM preview_leases AS p
      WHERE p.id = NEW.resource_id AND p.account_id = NEW.account_id
    )
    ON CONFLICT DO NOTHING;
  END IF;
  RETURN NEW;
END;
$$;


--
-- Name: preview_management_allowed(text, text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.preview_management_allowed(actor text, preview text) RETURNS boolean
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


--
-- Name: reject_audit_event_mutation(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.reject_audit_event_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF TG_OP = 'DELETE' AND OLD.resource_type = 'team'
     AND OLD.created_at < now() - interval '90 days' THEN
    RETURN OLD;
  END IF;
  RAISE EXCEPTION 'audit events are append-only within retention' USING ERRCODE = '55000';
END;
$$;


--
-- Name: reject_tunnel_config_identity_mutation(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.reject_tunnel_config_identity_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.tunnel_id IS DISTINCT FROM OLD.tunnel_id
     OR NEW.generation IS DISTINCT FROM OLD.generation
     OR NEW.previous_generation IS DISTINCT FROM OLD.previous_generation
     OR NEW.content_hash IS DISTINCT FROM OLD.content_hash
     OR NEW.snapshot IS DISTINCT FROM OLD.snapshot
     OR NEW.snapshot_reference IS DISTINCT FROM OLD.snapshot_reference
     OR NEW.created_by_actor_id IS DISTINCT FROM OLD.created_by_actor_id
     OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'immutable tunnel configuration generation fields cannot change'
      USING ERRCODE = '55000';
  END IF;
  RETURN NEW;
END;
$$;


--
-- Name: revoke_access_sessions_on_client_revocation(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.revoke_access_sessions_on_client_revocation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.state = 'active' AND NEW.state = 'revoked' THEN
        UPDATE user_machine_access_sessions
        SET state = 'revoked',
            revoked_at = coalesce(revoked_at, now()),
            updated_at = now(),
            revocation_reason = coalesce(revocation_reason, NEW.revocation_reason, 'client_revoked')
        WHERE cli_client_session_id = NEW.id AND state = 'active' AND revoked_at IS NULL;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: revoke_account_e2ee_key_dependents(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.revoke_account_e2ee_key_dependents() RETURNS trigger
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


--
-- Name: revoke_account_e2ee_keys_for_cli_session(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.revoke_account_e2ee_keys_for_cli_session() RETURNS trigger
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


--
-- Name: revoke_account_e2ee_keys_for_machine(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.revoke_account_e2ee_keys_for_machine() RETURNS trigger
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


--
-- Name: revoke_account_e2ee_keys_for_root(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.revoke_account_e2ee_keys_for_root() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL THEN
    UPDATE account_e2ee_keys
    SET revoked_at = NEW.revoked_at,
        revocation_reason = 'account_revoked',
        updated_at = NEW.updated_at
    WHERE user_id = NEW.user_id AND revoked_at IS NULL;
  END IF;
  RETURN NEW;
END;
$$;


--
-- Name: revoke_cli_peer_endpoint_for_session(text, text, timestamp with time zone, text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.revoke_cli_peer_endpoint_for_session(session_id text, session_user_id text, session_revoked_at timestamp with time zone, session_revocation_reason text) RETURNS void
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


--
-- Name: revoke_config_sync_for_environment(text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.revoke_config_sync_for_environment(target_environment text) RETURNS void
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_temp'
    AS $$
DECLARE
  revoked_time timestamptz := now();
BEGIN
  UPDATE control_config_credentials
  SET revoked_at = coalesce(revoked_at, revoked_time)
  WHERE environment_id = target_environment AND revoked_at IS NULL;

  UPDATE control_config_repository_access_operations
  SET state = 'revoked', revoked_at = coalesce(revoked_at, revoked_time), updated_at = revoked_time
  WHERE environment_id = target_environment AND revoked_at IS NULL;

  UPDATE control_config_repository_lease_authority
  SET lease_id = NULL, assignment_id = NULL, environment_id = NULL, machine_id = NULL,
      installation_generation = NULL, base_remote_revision = NULL, operation_id = NULL,
      acquired_at = NULL, expires_at = NULL, revoked_at = revoked_time,
      version = version + 1, updated_at = revoked_time
  WHERE environment_id = target_environment AND lease_id IS NOT NULL;
END;
$$;


--
-- Name: revoke_invalid_team_machine_access(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.revoke_invalid_team_machine_access() RETURNS trigger
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


--
-- Name: revoke_invalid_terminal_session_access(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.revoke_invalid_terminal_session_access() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE affected_team text; affected_session text; affected_account text; fence_all boolean := false; all_audience boolean := false;
BEGIN
 IF TG_TABLE_NAME='team_terminal_session_grants' THEN
  affected_team:=COALESCE(NEW.team_id,OLD.team_id); affected_session:=COALESCE(NEW.terminal_session_id,OLD.terminal_session_id);
  affected_account:=COALESCE(NEW.account_id,OLD.account_id); all_audience:=affected_account IS NULL;
 ELSIF TG_TABLE_NAME='team_resource_bindings' THEN affected_team:=COALESCE(NEW.team_id,OLD.team_id); affected_session:=COALESCE(NEW.resource_id,OLD.resource_id); fence_all:=true;
 ELSIF TG_TABLE_NAME='team_members' THEN affected_team:=COALESCE(NEW.team_id,OLD.team_id); affected_account:=COALESCE(NEW.account_id,OLD.account_id);
 ELSE affected_team:=COALESCE(NEW.team_id,OLD.team_id); fence_all:=true; END IF;
 UPDATE user_machine_access_sessions a SET state='revoked',revoked_at=now(),updated_at=now(),revocation_reason='terminal_session_permission_revoked'
 WHERE a.state='active' AND a.terminal_role IN ('viewer','interactive')
  AND (affected_team IS NULL OR a.team_id=affected_team) AND (affected_session IS NULL OR a.terminal_session_id=affected_session)
  AND (fence_all OR affected_account IS NULL OR a.user_id=affected_account)
  AND (TG_TABLE_NAME<>'team_terminal_session_grants' AND terminal_session_role(a.user_id,a.terminal_session_id) IS DISTINCT FROM a.terminal_role
   OR TG_TABLE_NAME='team_terminal_session_grants' AND (NOT all_audience OR NOT EXISTS(SELECT 1 FROM team_terminal_session_grants selected WHERE selected.team_id=a.team_id AND selected.terminal_session_id=a.terminal_session_id AND selected.audience='selected_member' AND selected.account_id=a.user_id)));
 RETURN NULL;
END $$;


--
-- Name: revoke_managed_ssh_client_key_on_session_revocation(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.revoke_managed_ssh_client_key_on_session_revocation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
	managed_reason text := CASE
		WHEN NEW.revocation_reason LIKE 'account_%' THEN 'account_revoked'
		WHEN NEW.revocation_reason = 'client_logout' THEN 'client_logout'
		ELSE 'client_revoked'
	END;
BEGIN
	UPDATE managed_ssh_client_keys
	SET state = 'revoked',
		revoked_at = coalesce(NEW.revoked_at, now()),
		revocation_reason = managed_reason,
		reconciliation_version = reconciliation_version + 1
	WHERE cli_client_session_id = NEW.id
		AND state = 'active';
	RETURN NEW;
END;
$$;


--
-- Name: revoke_unavailable_shared_terminal_target(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.revoke_unavailable_shared_terminal_target() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF NEW.state<>'online' OR NOT NEW.online OR NEW.revoked_at IS NOT NULL OR NEW.deleted_at IS NOT NULL OR NOT NEW.configured_capabilities @> ARRAY['terminal_host'] OR NOT NEW.observed_capabilities @> ARRAY['terminal_host'] THEN
  UPDATE user_machine_access_sessions SET state='revoked',revoked_at=now(),updated_at=now(),revocation_reason='terminal_host_unavailable'
   WHERE user_machine_id=NEW.id AND state='active' AND terminal_role IN ('viewer','interactive');
 END IF;
 RETURN NULL;
END $$;


--
-- Name: revoke_user_cli_client_sessions_on_status_change(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.revoke_user_cli_client_sessions_on_status_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
  revoked_time timestamptz := now();
  reason text := 'account_' || NEW.status;
BEGIN
  IF OLD.status = 'active' AND NEW.status <> 'active' THEN
    UPDATE paperboat.cli_client_sessions
    SET state = 'revoked',
        revoked_at = coalesce(revoked_at, revoked_time),
        revocation_reason = coalesce(revocation_reason, reason),
        version = version + 1
    WHERE user_id = NEW.id AND state = 'active';

    UPDATE paperboat.cli_access_tokens
    SET revoked_at = coalesce(revoked_at, revoked_time)
    WHERE cli_client_session_id IN (SELECT id FROM paperboat.cli_client_sessions WHERE user_id = NEW.id);

    UPDATE paperboat.cli_refresh_tokens
    SET state = 'revoked', revoked_at = coalesce(revoked_at, revoked_time)
    WHERE cli_client_session_id IN (SELECT id FROM paperboat.cli_client_sessions WHERE user_id = NEW.id)
      AND state <> 'revoked';
  END IF;
  RETURN NEW;
END;
$$;


--
-- Name: team_machine_capability_allowed(text, text, text, text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.team_machine_capability_allowed(actor text, team text, machine text, capability text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT team_paid_entitled(team)
 AND capability=ANY(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage','private_access']) AND EXISTS (
  SELECT 1 FROM teams t JOIN team_members member USING(team_id)
  JOIN team_resource_bindings b USING(team_id)
  JOIN user_machines m ON m.id=b.resource_id
  JOIN team_machine_grants g ON g.team_id=b.team_id AND g.machine_id=m.id
  WHERE t.team_id=team AND t.deleted_at IS NULL AND member.account_id=actor AND member.active
   AND b.resource_kind='machine' AND b.active AND m.id=machine AND m.revoked_at IS NULL AND m.deleted_at IS NULL
   AND ((m.owner_team_id IS NULL AND b.owner_account=m.user_id) OR m.owner_team_id=t.team_id)
   AND g.active AND (g.audience='all_members' OR g.account_id=actor) AND capability=ANY(g.capabilities)
 );
$$;


--
-- Name: team_paid_entitled(text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.team_paid_entitled(team text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT EXISTS (
  SELECT 1 FROM billing_workspaces w JOIN teams t ON t.team_id=w.team_id
  WHERE w.team_id=team AND w.user_id IS NULL AND NOT t.suspended AND t.deleted_at IS NULL
   AND w.state IN ('active','trialing')
   AND w.period_start IS NOT NULL AND w.period_start<=now()
   AND w.period_end IS NOT NULL AND now()<w.period_end
 );
$$;


--
-- Name: terminal_session_role(text, text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.terminal_session_role(actor text, session text) RETURNS text
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT CASE
  WHEN EXISTS(SELECT 1 FROM user_machine_terminal_sessions s WHERE s.id=session AND s.owner_account=actor AND s.deleted_at IS NULL) THEN 'owner'
  ELSE (SELECT CASE WHEN selected.account_id IS NOT NULL THEN CASE WHEN selected.active THEN selected.role END ELSE all_grant.role END
   FROM team_resource_bindings b
   JOIN teams t USING(team_id)
   JOIN team_members m ON m.team_id=b.team_id AND m.account_id=actor AND m.active
   LEFT JOIN team_terminal_session_grants selected ON selected.team_id=b.team_id AND selected.terminal_session_id=b.resource_id AND selected.audience='selected_member' AND selected.account_id=actor
   LEFT JOIN team_terminal_session_grants all_grant ON all_grant.team_id=b.team_id AND all_grant.terminal_session_id=b.resource_id AND all_grant.audience='all_members' AND all_grant.active
   WHERE b.resource_kind='terminal_session' AND b.resource_id=session AND b.active AND t.deleted_at IS NULL
    AND team_paid_entitled(b.team_id)
    AND (selected.account_id IS NOT NULL OR all_grant.team_id IS NOT NULL)
   ORDER BY CASE WHEN selected.account_id IS NOT NULL THEN 0 ELSE 1 END LIMIT 1)
 END;
$$;


--
-- Name: tunnel_certificate_issuance_lock_target_guard_v1(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.tunnel_certificate_issuance_lock_target_guard_v1() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM tunnel_domains WHERE id = NEW.domain_id)
     AND NOT EXISTS (SELECT 1 FROM preview_domains WHERE id = NEW.domain_id)
     AND NOT EXISTS (SELECT 1 FROM tunnel_platform_certificate_targets WHERE id = NEW.domain_id) THEN
    RAISE EXCEPTION 'certificate issuance lock target does not exist';
  END IF;
  RETURN NEW;
END
$$;


--
-- Name: tunnel_certificate_target_guard_v1(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.tunnel_certificate_target_guard_v1() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.target_kind = 'platform_wildcard' THEN
    IF NOT EXISTS (
      SELECT 1
      FROM tunnel_platform_certificate_targets AS platform
      WHERE platform.id = NEW.domain_id
        AND platform.account_id = NEW.account_id
        AND platform.hostname = NEW.hostname
        AND platform.generation = NEW.domain_generation
        AND NEW.tunnel_id IS NULL
        AND NEW.route_id IS NULL
        AND NEW.leaf_hostname IS NULL
    ) THEN
      RAISE EXCEPTION 'certificate platform target does not match target projection';
    END IF;
  ELSIF NEW.target_kind = 'durable_route' THEN
    IF NOT EXISTS (
      SELECT 1 FROM tunnel_domains AS d
      WHERE d.id = NEW.domain_id
        AND d.account_id = NEW.account_id
        AND d.tunnel_id = NEW.tunnel_id
    ) THEN
      RAISE EXCEPTION 'certificate durable target does not match domain';
    END IF;
  ELSIF NEW.target_kind = 'preview_lease' THEN
    IF NOT EXISTS (
      SELECT 1 FROM preview_domains AS d
      WHERE d.id = NEW.domain_id
        AND d.account_id = NEW.account_id
        AND d.preview_id = NEW.preview_id
    ) THEN
      RAISE EXCEPTION 'certificate preview target does not match domain';
    END IF;
  END IF;
  RETURN NEW;
END
$$;


--
-- Name: tunnel_certificate_user_account_fill_v1(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.tunnel_certificate_user_account_fill_v1() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.target_kind <> 'platform_wildcard' AND NEW.user_account_id IS NULL THEN
    NEW.user_account_id := NEW.account_id;
  END IF;
  RETURN NEW;
END
$$;


--
-- Name: tunnel_management_allowed(text, text); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.tunnel_management_allowed(actor text, tunnel text) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 SELECT EXISTS(SELECT 1 FROM tunnels t JOIN user_machines m ON m.id=t.created_by_host_id
 WHERE t.id=tunnel AND (
  (t.account_id=actor AND m.owner_team_id IS NULL AND m.user_id=actor)
  OR (t.access_mode IN ('private','team') AND t.deleted_at IS NULL
   AND m.configured_capabilities @> ARRAY['preview_launch']::text[]
   AND machine_capability_allowed(actor,m.id,'tunnel_manage')
   AND NOT EXISTS(SELECT 1 FROM tunnel_connectors c LEFT JOIN user_machines other ON other.id=c.host_id
    WHERE c.tunnel_id=t.id AND c.revoked_at IS NULL AND
     (other.id IS NULL OR NOT machine_capability_allowed(actor,other.id,'tunnel_manage') OR NOT other.configured_capabilities @> ARRAY['preview_launch']::text[]))
  )
 ));
$$;


--
-- Name: tunnel_public_listener_capacity_guard_v1(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.tunnel_public_listener_capacity_guard_v1() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF NEW.protocol='tcp' AND (NEW.public_tcp_listener_id IS NULL OR NEW.public_tcp_port IS NULL) THEN
 RAISE EXCEPTION 'public listener capacity is exhausted' USING ERRCODE='23514',CONSTRAINT='tunnel_routes_public_listener_capacity_check';
 END IF;
 RETURN NEW;
END $$;


--
-- Name: validate_connector_rotation_target_scope(); Type: FUNCTION; Schema: paperboat; Owner: -
--

CREATE FUNCTION paperboat.validate_connector_rotation_target_scope() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
  operation_account text;
  operation_resource text;
  operation_kind text;
BEGIN
  SELECT account_id, resource_id, resource_kind
    INTO operation_account, operation_resource, operation_kind
  FROM operations
  WHERE id = NEW.operation_id;
  IF operation_account IS NULL
     OR operation_account <> NEW.account_id
     OR operation_resource <> NEW.tunnel_id
     OR operation_kind <> 'tunnel' THEN
    RAISE EXCEPTION 'connector rotation target operation scope mismatch' USING ERRCODE = '22023';
  END IF;
  IF EXISTS (
    SELECT 1 FROM operations
    WHERE id = NEW.operation_id
      AND operation_type <> 'connector.credentials.rotate'
  ) THEN
    RAISE EXCEPTION 'connector rotation target operation type mismatch' USING ERRCODE = '22023';
  END IF;
  IF TG_OP = 'UPDATE' AND (
    NEW.operation_id IS DISTINCT FROM OLD.operation_id
    OR NEW.account_id IS DISTINCT FROM OLD.account_id
    OR NEW.tunnel_id IS DISTINCT FROM OLD.tunnel_id
    OR NEW.connector_id IS DISTINCT FROM OLD.connector_id
    OR NEW.host_id IS DISTINCT FROM OLD.host_id
    OR NEW.target_set_hash IS DISTINCT FROM OLD.target_set_hash
    OR NEW.old_credential_generation IS DISTINCT FROM OLD.old_credential_generation
    OR NEW.new_credential_generation IS DISTINCT FROM OLD.new_credential_generation
    OR (OLD.overlap_until IS NOT NULL AND NEW.overlap_until IS DISTINCT FROM OLD.overlap_until)
    OR (OLD.new_credential_valid_until IS NOT NULL AND NEW.new_credential_valid_until IS DISTINCT FROM OLD.new_credential_valid_until)
  ) THEN
    RAISE EXCEPTION 'connector rotation target identity and policy are immutable' USING ERRCODE = '55000';
  END IF;
  IF EXISTS (
    SELECT 1 FROM tunnel_connector_rotation_targets
    WHERE operation_id = NEW.operation_id
      AND target_set_hash <> NEW.target_set_hash
  ) THEN
    RAISE EXCEPTION 'connector rotation target set hash is immutable' USING ERRCODE = '55000';
  END IF;
  RETURN NEW;
END;
$$;


SET LOCAL default_tablespace = '';

SET LOCAL default_table_access_method = heap;

--
-- Name: account_e2ee_keys; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.account_e2ee_keys (
    key_id text NOT NULL,
    user_id text NOT NULL,
    public_key bytea NOT NULL,
    fingerprint bytea NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    cli_client_session_id text,
    user_machine_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    revocation_reason text,
    CONSTRAINT account_e2ee_keys_check CHECK (((revoked_at IS NULL) = (revocation_reason IS NULL))),
    CONSTRAINT account_e2ee_keys_check1 CHECK (((revoked_at IS NULL) OR (revoked_at >= created_at))),
    CONSTRAINT account_e2ee_keys_fingerprint_check CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT account_e2ee_keys_generation_check CHECK ((generation > 0)),
    CONSTRAINT account_e2ee_keys_key_id_check CHECK ((key_id ~ '^aek_[A-Za-z0-9_-]{16,128}$'::text)),
    CONSTRAINT account_e2ee_keys_public_key_check CHECK ((octet_length(public_key) = 32)),
    CONSTRAINT account_e2ee_keys_revocation_reason_check CHECK (((revocation_reason IS NULL) OR (revocation_reason = ANY (ARRAY['device_removed'::text, 'key_compromise'::text, 'account_revoked'::text]))))
);


--
-- Name: account_e2ee_roots; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.account_e2ee_roots (
    user_id text NOT NULL,
    public_key bytea NOT NULL,
    fingerprint bytea NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    CONSTRAINT account_e2ee_roots_check CHECK (((revoked_at IS NULL) OR (revoked_at >= created_at))),
    CONSTRAINT account_e2ee_roots_fingerprint_check CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT account_e2ee_roots_generation_check CHECK ((generation > 0)),
    CONSTRAINT account_e2ee_roots_public_key_check CHECK ((octet_length(public_key) = 32))
);


--
-- Name: account_network_preferences; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.account_network_preferences (
    account_id text NOT NULL,
    revision bigint NOT NULL,
    device_suffix text,
    device_loopback_cidr text,
    selected_team_id text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT account_network_preferences_revision_check CHECK (((revision >= 1) AND (revision <= '9007199254740991'::bigint)))
);


--
-- Name: audit_events; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.audit_events (
    id text NOT NULL,
    actor_user_id text,
    actor_type text NOT NULL,
    event_type text NOT NULL,
    resource_type text NOT NULL,
    resource_id text DEFAULT ''::text NOT NULL,
    idempotency_key text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    account_id text,
    actor_id text,
    change_type text DEFAULT 'legacy'::text NOT NULL,
    outcome text DEFAULT 'changed'::text NOT NULL,
    request_id text,
    correlation_id text,
    source_device_id text,
    cursor_sequence bigint NOT NULL,
    CONSTRAINT audit_events_outcome_check CHECK ((outcome = ANY (ARRAY['unchanged'::text, 'changed'::text, 'uncertain'::text])))
);


--
-- Name: audit_events_cursor_sequence_seq; Type: SEQUENCE; Schema: paperboat; Owner: -
--

ALTER TABLE paperboat.audit_events ALTER COLUMN cursor_sequence ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME paperboat.audit_events_cursor_sequence_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: auth_enrollment_redemptions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.auth_enrollment_redemptions (
    enrollment_id text NOT NULL,
    cli_client_session_id text NOT NULL,
    verifier_hash bytea NOT NULL,
    response_ciphertext bytea NOT NULL,
    access_expires_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT auth_enrollment_redemptions_verifier_hash_check CHECK ((octet_length(verifier_hash) = 32))
);


--
-- Name: auth_rate_limits; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.auth_rate_limits (
    bucket_key text NOT NULL,
    window_start timestamp with time zone NOT NULL,
    request_count integer NOT NULL
);


--
-- Name: backup_reports; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.backup_reports (
    database_id text NOT NULL,
    run_id text NOT NULL,
    kind text NOT NULL,
    backup_run_id text DEFAULT ''::text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone NOT NULL,
    outcome text NOT NULL,
    failure_code text DEFAULT ''::text NOT NULL,
    bytes bigint NOT NULL,
    sha256 text DEFAULT ''::text NOT NULL,
    retained_until timestamp with time zone,
    request_hash bytea NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT backup_reports_bytes_check CHECK ((bytes >= 0)),
    CONSTRAINT backup_reports_check CHECK ((completed_at >= started_at)),
    CONSTRAINT backup_reports_database_id_check CHECK (((length(database_id) >= 1) AND (length(database_id) <= 64))),
    CONSTRAINT backup_reports_kind_check CHECK ((kind = ANY (ARRAY['backup'::text, 'restore'::text]))),
    CONSTRAINT backup_reports_outcome_check CHECK ((outcome = ANY (ARRAY['succeeded'::text, 'failed'::text]))),
    CONSTRAINT backup_reports_request_hash_check CHECK ((octet_length(request_hash) = 32)),
    CONSTRAINT backup_reports_run_id_check CHECK (((length(run_id) >= 1) AND (length(run_id) <= 128)))
);


--
-- Name: billing_checkout_reservations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.billing_checkout_reservations (
    id text NOT NULL,
    user_id text NOT NULL,
    product_code text NOT NULL,
    idempotency_key text NOT NULL,
    provider_checkout_id text,
    checkout_url text,
    state text DEFAULT 'reserved'::text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    last_error text,
    uncertain_at timestamp with time zone
);


--
-- Name: billing_operator_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.billing_operator_operations (
    idempotency_key text NOT NULL,
    actor_user_id text NOT NULL,
    operation_type text NOT NULL,
    target text NOT NULL,
    request_hash text NOT NULL,
    result jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT billing_operator_operations_operation_type_check CHECK ((operation_type = ANY (ARRAY['put_product'::text, 'put_policy'::text])))
);


--
-- Name: billing_operator_policy; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.billing_operator_policy (
    singleton boolean DEFAULT true NOT NULL,
    personal_allowance_bytes bigint,
    team_allowance_bytes bigint,
    personal_machine_limit integer,
    revision bigint DEFAULT 1 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT billing_operator_policy_personal_allowance_bytes_check CHECK (((personal_allowance_bytes IS NULL) OR (personal_allowance_bytes >= 0))),
    CONSTRAINT billing_operator_policy_personal_machine_limit_check CHECK (((personal_machine_limit IS NULL) OR (personal_machine_limit >= 0))),
    CONSTRAINT billing_operator_policy_revision_check CHECK ((revision > 0)),
    CONSTRAINT billing_operator_policy_singleton_check CHECK (singleton),
    CONSTRAINT billing_operator_policy_team_allowance_bytes_check CHECK (((team_allowance_bytes IS NULL) OR (team_allowance_bytes >= 0)))
);


--
-- Name: billing_portal_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.billing_portal_operations (
    idempotency_key text NOT NULL,
    user_id text NOT NULL,
    request_hash bytea NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    result_ciphertext bytea,
    last_error text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT billing_portal_operations_check CHECK (((state = 'succeeded'::text) = (result_ciphertext IS NOT NULL))),
    CONSTRAINT billing_portal_operations_idempotency_key_check CHECK (((length(TRIM(BOTH FROM idempotency_key)) >= 1) AND (length(TRIM(BOTH FROM idempotency_key)) <= 256))),
    CONSTRAINT billing_portal_operations_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'succeeded'::text, 'failed'::text, 'uncertain'::text])))
);


--
-- Name: billing_products; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.billing_products (
    id text NOT NULL,
    code text NOT NULL,
    provider text NOT NULL,
    provider_product_id text NOT NULL,
    provider_price_id text NOT NULL,
    catalog_type text NOT NULL,
    catalog_ref text NOT NULL,
    active boolean DEFAULT true NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    verified_unit_amount bigint,
    verified_currency text,
    verified_interval text,
    CONSTRAINT billing_products_verified_price_valid CHECK ((((verified_unit_amount IS NULL) AND (verified_currency IS NULL) AND (verified_interval IS NULL)) OR ((verified_unit_amount >= 0) AND (verified_currency ~ '^[a-z]{3}$'::text) AND (verified_interval = ANY (ARRAY['month'::text, 'year'::text])))))
);


--
-- Name: billing_uncertain_recoveries; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.billing_uncertain_recoveries (
    idempotency_key text NOT NULL,
    operation_kind text NOT NULL,
    operation_id text NOT NULL,
    request_hash bytea NOT NULL,
    actor_user_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT billing_uncertain_recoveries_idempotency_key_check CHECK (((length(TRIM(BOTH FROM idempotency_key)) >= 8) AND (length(TRIM(BOTH FROM idempotency_key)) <= 256))),
    CONSTRAINT billing_uncertain_recoveries_operation_id_check CHECK (((length(TRIM(BOTH FROM operation_id)) >= 1) AND (length(TRIM(BOTH FROM operation_id)) <= 256))),
    CONSTRAINT billing_uncertain_recoveries_operation_kind_check CHECK ((operation_kind = ANY (ARRAY['checkout'::text, 'portal'::text]))),
    CONSTRAINT billing_uncertain_recoveries_request_hash_check CHECK ((octet_length(request_hash) = 32))
);


--
-- Name: billing_workspace_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.billing_workspace_operations (
    idempotency_key text NOT NULL,
    workspace_id text NOT NULL,
    actor_user_id text NOT NULL,
    kind text NOT NULL,
    request_hash text NOT NULL,
    state text NOT NULL,
    result_ciphertext bytea,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    request_ciphertext bytea,
    provider_attempted_at timestamp with time zone,
    CONSTRAINT billing_workspace_operations_kind_check CHECK ((kind = ANY (ARRAY['customer'::text, 'checkout'::text, 'portal'::text, 'seats'::text]))),
    CONSTRAINT billing_workspace_operations_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'succeeded'::text, 'failed'::text, 'uncertain'::text])))
);


--
-- Name: billing_workspace_recoveries; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.billing_workspace_recoveries (
    idempotency_key text NOT NULL,
    operation_key text NOT NULL,
    workspace_id text NOT NULL,
    actor_user_id text NOT NULL,
    disposition text NOT NULL,
    request_hash bytea NOT NULL,
    evidence_sha256 bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT billing_workspace_recoveries_disposition_check CHECK ((disposition = ANY (ARRAY['not_applied'::text, 'superseded_portal'::text, 'recovered_checkout'::text, 'expired_checkout'::text]))),
    CONSTRAINT billing_workspace_recoveries_evidence_sha256_check CHECK ((octet_length(evidence_sha256) = 32)),
    CONSTRAINT billing_workspace_recoveries_request_hash_check CHECK ((octet_length(request_hash) = 32))
);


--
-- Name: billing_workspace_usage_periods; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.billing_workspace_usage_periods (
    workspace_id text NOT NULL,
    period_start timestamp with time zone NOT NULL,
    period_end timestamp with time zone NOT NULL,
    included_bytes bigint,
    consumed_bytes bigint DEFAULT 0 NOT NULL,
    observed_bytes bigint DEFAULT 0 NOT NULL,
    CONSTRAINT billing_workspace_usage_periods_check CHECK ((period_end > period_start)),
    CONSTRAINT billing_workspace_usage_periods_consumed_bytes_check CHECK ((consumed_bytes >= 0)),
    CONSTRAINT billing_workspace_usage_periods_included_bytes_check CHECK ((included_bytes >= 0)),
    CONSTRAINT billing_workspace_usage_periods_observed_bytes_check CHECK ((observed_bytes >= 0))
);


--
-- Name: billing_workspaces; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.billing_workspaces (
    id text NOT NULL,
    user_id text,
    team_id text,
    provider_customer_id text,
    provider_subscription_id text,
    provider_item_id text,
    product_code text,
    state text DEFAULT 'free'::text NOT NULL,
    quantity integer DEFAULT 0 NOT NULL,
    period_start timestamp with time zone,
    period_end timestamp with time zone,
    cancel_at_period_end boolean DEFAULT false NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT billing_workspaces_check CHECK (((((user_id IS NOT NULL))::integer + ((team_id IS NOT NULL))::integer) = 1)),
    CONSTRAINT billing_workspaces_check1 CHECK (((period_start IS NULL) OR (period_end > period_start))),
    CONSTRAINT billing_workspaces_quantity_check CHECK ((quantity >= 0)),
    CONSTRAINT billing_workspaces_state_check CHECK ((state = ANY (ARRAY['free'::text, 'incomplete'::text, 'incomplete_expired'::text, 'trialing'::text, 'active'::text, 'past_due'::text, 'canceled'::text, 'unpaid'::text, 'paused'::text])))
);


--
-- Name: browser_access_sessions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.browser_access_sessions (
    grant_id text NOT NULL,
    token_hash bytea NOT NULL,
    account_id text NOT NULL,
    hostname text NOT NULL,
    resource_kind text NOT NULL,
    resource_id text NOT NULL,
    resource_generation bigint NOT NULL,
    preview_owner_session_id text,
    owner_account_id text NOT NULL,
    access_mode text NOT NULL,
    team_id text,
    team_generation bigint,
    membership_generation bigint,
    binding_generation bigint,
    grant_generation bigint,
    trusted_session_id text NOT NULL,
    trusted_session_version bigint NOT NULL,
    trusted_session_expires_at timestamp with time zone NOT NULL,
    issued_at timestamp with time zone NOT NULL,
    last_seen_at timestamp with time zone NOT NULL,
    absolute_expires_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    CONSTRAINT browser_access_sessions_access_mode_check CHECK ((access_mode = ANY (ARRAY['private'::text, 'team'::text]))),
    CONSTRAINT browser_access_sessions_binding_generation_check CHECK ((binding_generation > 0)),
    CONSTRAINT browser_access_sessions_check1 CHECK ((((access_mode = 'private'::text) AND (team_id IS NULL) AND (team_generation IS NULL) AND (membership_generation IS NULL) AND (binding_generation IS NULL) AND (grant_generation IS NULL)) OR ((access_mode = 'team'::text) AND (team_id IS NOT NULL) AND (team_generation IS NOT NULL) AND (membership_generation IS NOT NULL) AND (binding_generation IS NOT NULL) AND (grant_generation IS NOT NULL)))),
    CONSTRAINT browser_access_sessions_grant_generation_check CHECK ((grant_generation > 0)),
    CONSTRAINT browser_access_sessions_membership_generation_check CHECK ((membership_generation > 0)),
    CONSTRAINT browser_access_sessions_resource_generation_check CHECK ((resource_generation > 0)),
    CONSTRAINT browser_access_sessions_resource_kind_check CHECK ((resource_kind = ANY (ARRAY['preview'::text, 'tunnel'::text, 'lazy_policy'::text]))),
    CONSTRAINT browser_access_sessions_team_authority_check CHECK ((((team_id IS NULL) AND (team_generation IS NULL) AND (membership_generation IS NULL) AND (binding_generation IS NULL) AND (grant_generation IS NULL)) OR ((team_id IS NOT NULL) AND (team_generation IS NOT NULL) AND (membership_generation IS NOT NULL) AND (binding_generation IS NOT NULL) AND (grant_generation IS NOT NULL)))),
    CONSTRAINT browser_access_sessions_team_generation_check CHECK ((team_generation > 0)),
    CONSTRAINT browser_access_sessions_token_hash_check CHECK ((octet_length(token_hash) = 32)),
    CONSTRAINT browser_access_sessions_trusted_session_version_check CHECK ((trusted_session_version > 0))
);


--
-- Name: browser_access_transactions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.browser_access_transactions (
    transaction_id text NOT NULL,
    state_hash bytea NOT NULL,
    hostname text NOT NULL,
    return_path text NOT NULL,
    account_id text,
    resource_kind text,
    resource_id text,
    resource_generation bigint,
    preview_owner_session_id text,
    owner_account_id text,
    access_mode text,
    team_id text,
    team_generation bigint,
    membership_generation bigint,
    binding_generation bigint,
    grant_generation bigint,
    trusted_session_id text,
    trusted_session_version bigint,
    trusted_session_expires_at timestamp with time zone,
    handoff_hash bytea,
    handoff_expires_at timestamp with time zone,
    issued_at timestamp with time zone,
    redeemed_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT browser_access_transactions_access_mode_check CHECK ((access_mode = ANY (ARRAY['private'::text, 'team'::text]))),
    CONSTRAINT browser_access_transactions_binding_generation_check CHECK ((binding_generation > 0)),
    CONSTRAINT browser_access_transactions_check CHECK ((expires_at > created_at)),
    CONSTRAINT browser_access_transactions_check1 CHECK (((issued_at IS NULL) = (account_id IS NULL))),
    CONSTRAINT browser_access_transactions_check2 CHECK (((issued_at IS NULL) OR ((resource_id IS NOT NULL) AND (handoff_hash IS NOT NULL) AND (handoff_expires_at > issued_at)))),
    CONSTRAINT browser_access_transactions_grant_generation_check CHECK ((grant_generation > 0)),
    CONSTRAINT browser_access_transactions_handoff_hash_check CHECK (((handoff_hash IS NULL) OR (octet_length(handoff_hash) = 32))),
    CONSTRAINT browser_access_transactions_hostname_check CHECK (((hostname = lower(hostname)) AND (hostname !~ '[:/\\\\[:cntrl:]]'::text))),
    CONSTRAINT browser_access_transactions_membership_generation_check CHECK ((membership_generation > 0)),
    CONSTRAINT browser_access_transactions_resource_generation_check CHECK ((resource_generation > 0)),
    CONSTRAINT browser_access_transactions_resource_kind_check CHECK ((resource_kind = ANY (ARRAY['preview'::text, 'tunnel'::text, 'lazy_policy'::text]))),
    CONSTRAINT browser_access_transactions_return_path_check CHECK (((octet_length(return_path) >= 1) AND (octet_length(return_path) <= 2048))),
    CONSTRAINT browser_access_transactions_state_hash_check CHECK ((octet_length(state_hash) = 32)),
    CONSTRAINT browser_access_transactions_team_generation_check CHECK ((team_generation > 0)),
    CONSTRAINT browser_access_transactions_trusted_session_version_check CHECK ((trusted_session_version > 0))
);


--
-- Name: browser_machine_credentials; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.browser_machine_credentials (
    credential_id text NOT NULL,
    token_hash bytea NOT NULL,
    account_id text NOT NULL,
    hostname text NOT NULL,
    resource_kind text NOT NULL,
    resource_id text NOT NULL,
    route_id text NOT NULL,
    action text NOT NULL,
    resource_generation bigint NOT NULL,
    preview_owner_session_id text,
    owner_account_id text NOT NULL,
    access_mode text NOT NULL,
    team_id text,
    team_generation bigint,
    membership_generation bigint,
    binding_generation bigint,
    grant_generation bigint,
    issued_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    CONSTRAINT browser_machine_credentials_access_mode_check CHECK ((access_mode = ANY (ARRAY['private'::text, 'team'::text]))),
    CONSTRAINT browser_machine_credentials_action_check CHECK ((action = 'use'::text)),
    CONSTRAINT browser_machine_credentials_check CHECK (((expires_at > issued_at) AND (expires_at <= (issued_at + '00:05:00'::interval)))),
    CONSTRAINT browser_machine_credentials_resource_generation_check CHECK ((resource_generation > 0)),
    CONSTRAINT browser_machine_credentials_resource_kind_check CHECK ((resource_kind = ANY (ARRAY['preview'::text, 'tunnel'::text, 'lazy_policy'::text]))),
    CONSTRAINT browser_machine_credentials_team_authority_check CHECK ((((team_id IS NULL) AND (team_generation IS NULL) AND (membership_generation IS NULL) AND (binding_generation IS NULL) AND (grant_generation IS NULL)) OR ((team_id IS NOT NULL) AND (team_generation > 0) AND (membership_generation > 0) AND (binding_generation > 0) AND (grant_generation > 0)))),
    CONSTRAINT browser_machine_credentials_token_hash_check CHECK ((octet_length(token_hash) = 32))
);


--
-- Name: cli_access_tokens; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.cli_access_tokens (
    token_hash text NOT NULL,
    cli_client_session_id text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone
);


--
-- Name: cli_client_sessions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.cli_client_sessions (
    id text NOT NULL,
    user_id text NOT NULL,
    client_id text NOT NULL,
    client_label text NOT NULL,
    device_type text NOT NULL,
    os text NOT NULL,
    scopes text[] NOT NULL,
    state text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    approved_at timestamp with time zone NOT NULL,
    last_used_at timestamp with time zone,
    revoked_at timestamp with time zone,
    revocation_reason text,
    version bigint DEFAULT 1 NOT NULL,
    fresh_e2ee_bootstrap boolean DEFAULT false NOT NULL,
    user_machine_id text,
    CONSTRAINT cli_client_sessions_state_check CHECK ((state = ANY (ARRAY['active'::text, 'revoked'::text])))
);


--
-- Name: cli_refresh_tokens; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.cli_refresh_tokens (
    token_hash text NOT NULL,
    cli_client_session_id text NOT NULL,
    state text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone NOT NULL,
    rotated_at timestamp with time zone,
    revoked_at timestamp with time zone,
    CONSTRAINT cli_refresh_tokens_state_check CHECK ((state = ANY (ARRAY['active'::text, 'rotated'::text, 'revoked'::text])))
);


--
-- Name: codex_sessions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.codex_sessions (
    id text NOT NULL,
    environment_id text NOT NULL,
    machine_id text NOT NULL,
    user_id text NOT NULL,
    cli_client_session_id text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash bytea NOT NULL,
    state text DEFAULT 'preparing'::text NOT NULL,
    installation_generation bigint NOT NULL,
    connector_id text DEFAULT 'runtime'::text NOT NULL,
    connector_generation bigint NOT NULL,
    edge_pool text NOT NULL,
    edge_node_id text NOT NULL,
    edge_assignment_host text NOT NULL,
    remote_codex_version text,
    failure_code text,
    cleanup_status text DEFAULT 'not_requested'::text NOT NULL,
    lease_expires_at timestamp with time zone NOT NULL,
    prepared_at timestamp with time zone,
    reconnecting_at timestamp with time zone,
    stopping_at timestamp with time zone,
    stopped_at timestamp with time zone,
    last_renewed_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT codex_sessions_cleanup_status_check CHECK ((cleanup_status = ANY (ARRAY['not_requested'::text, 'pending'::text, 'complete'::text, 'uncertain'::text]))),
    CONSTRAINT codex_sessions_connector_generation_check CHECK ((connector_generation > 0)),
    CONSTRAINT codex_sessions_failure_code_check CHECK (((failure_code IS NULL) OR ((length(failure_code) >= 1) AND (length(failure_code) <= 64)))),
    CONSTRAINT codex_sessions_installation_generation_check CHECK ((installation_generation > 0)),
    CONSTRAINT codex_sessions_state_check CHECK ((state = ANY (ARRAY['preparing'::text, 'ready'::text, 'reconnecting'::text, 'stopping'::text, 'stopped'::text, 'failed'::text])))
);


--
-- Name: connector_proof_replays; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.connector_proof_replays (
    account_id text NOT NULL,
    tunnel_id text NOT NULL,
    connector_id text NOT NULL,
    credential_generation bigint NOT NULL,
    proof_kind text NOT NULL,
    nonce text NOT NULL,
    proof_digest bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT connector_proof_replays_check CHECK ((expires_at > created_at)),
    CONSTRAINT connector_proof_replays_credential_generation_check CHECK ((credential_generation > 0)),
    CONSTRAINT connector_proof_replays_nonce_check CHECK (((length(TRIM(BOTH FROM nonce)) >= 16) AND (length(TRIM(BOTH FROM nonce)) <= 128))),
    CONSTRAINT connector_proof_replays_proof_digest_check CHECK ((octet_length(proof_digest) = 32)),
    CONSTRAINT connector_proof_replays_proof_kind_check CHECK ((proof_kind = ANY (ARRAY['auth'::text, 'renew'::text, 'rotation'::text])))
);


--
-- Name: control_config_assignments; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_config_assignments (
    id text NOT NULL,
    environment_id text NOT NULL,
    repository_id text,
    mode text DEFAULT 'pull_only'::text NOT NULL,
    consent_state text DEFAULT 'not_required'::text NOT NULL,
    warning_revision text,
    accepted_at timestamp with time zone,
    revoked_at timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    machine_id text NOT NULL,
    push_repository_id text,
    automatic_updates boolean DEFAULT false NOT NULL,
    adopted_team_id text,
    adopted_default_version bigint,
    approved_pull_revision text,
    approved_at timestamp with time zone,
    CONSTRAINT control_config_assignment_adoption_complete CHECK ((((adopted_team_id IS NULL) AND (adopted_default_version IS NULL)) OR ((adopted_team_id IS NOT NULL) AND (adopted_default_version IS NOT NULL) AND (adopted_default_version > 0)))),
    CONSTRAINT control_config_assignments_check CHECK ((((consent_state = 'pending'::text) AND (warning_revision IS NOT NULL)) OR (consent_state <> 'pending'::text))),
    CONSTRAINT control_config_assignments_check1 CHECK ((((consent_state = 'accepted'::text) AND (warning_revision IS NOT NULL) AND (accepted_at IS NOT NULL)) OR (consent_state <> 'accepted'::text))),
    CONSTRAINT control_config_assignments_consent_state_check CHECK ((consent_state = ANY (ARRAY['not_required'::text, 'pending'::text, 'accepted'::text, 'revoked'::text]))),
    CONSTRAINT control_config_assignments_mode_check CHECK ((mode = ANY (ARRAY['pull_only'::text, 'push_only'::text, 'bidirectional'::text])))
);


--
-- Name: control_config_conflict_resolutions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_config_conflict_resolutions (
    id text NOT NULL,
    environment_id text NOT NULL,
    repository_id text NOT NULL,
    assignment_id text NOT NULL,
    conflict_revision text NOT NULL,
    path text NOT NULL,
    scope text DEFAULT 'path'::text NOT NULL,
    action text NOT NULL,
    expected_remote_revision text NOT NULL,
    requested_by_user_id text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    landed_revision text,
    requested_at timestamp with time zone DEFAULT now() NOT NULL,
    applied_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_config_conflict_resolutions_action_check CHECK ((action = ANY (ARRAY['keep_local'::text, 'keep_remote'::text, 'force_pull'::text, 'force_push'::text]))),
    CONSTRAINT control_config_conflict_resolutions_check CHECK ((((scope = 'path'::text) AND (path <> '.'::text)) OR ((scope = 'config'::text) AND (path = '.'::text)))),
    CONSTRAINT control_config_conflict_resolutions_check1 CHECK (((scope = 'path'::text) OR (action = ANY (ARRAY['force_pull'::text, 'force_push'::text])))),
    CONSTRAINT control_config_conflict_resolutions_conflict_revision_check CHECK ((length(conflict_revision) = 64)),
    CONSTRAINT control_config_conflict_resolutions_scope_check CHECK ((scope = ANY (ARRAY['path'::text, 'config'::text]))),
    CONSTRAINT control_config_conflict_resolutions_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'applied'::text, 'rejected'::text])))
);


--
-- Name: control_config_credentials; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_config_credentials (
    jti_hash bytea NOT NULL,
    jti text NOT NULL,
    operation_key text NOT NULL,
    request_hash bytea NOT NULL,
    environment_id text NOT NULL,
    machine_id text NOT NULL,
    assignment_id text NOT NULL,
    warning_revision text,
    credential_ciphertext bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: control_config_repositories; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_config_repositories (
    id text NOT NULL,
    owner_user_id text NOT NULL,
    provider text NOT NULL,
    external_ref text NOT NULL,
    display_name text NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    provider_account_id text,
    external_repository_id text,
    clone_url text,
    publish_url text,
    default_branch text,
    authorization_ref text,
    credential_capability text,
    observed_revision text,
    disconnected_at timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    CONSTRAINT control_config_repositories_state_check CHECK ((state = ANY (ARRAY['active'::text, 'disconnected'::text, 'revoked'::text, 'quarantined'::text])))
);


--
-- Name: control_config_repository_access_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_config_repository_access_operations (
    operation_id text NOT NULL,
    request_hash bytea NOT NULL,
    repository_id text NOT NULL,
    assignment_id text NOT NULL,
    environment_id text NOT NULL,
    machine_id text NOT NULL,
    installation_generation bigint NOT NULL,
    warning_revision text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    access_ciphertext bytea,
    expires_at timestamp with time zone,
    revoked_at timestamp with time zone,
    provider_revoked_at timestamp with time zone,
    revoke_attempts integer DEFAULT 0 NOT NULL,
    last_error_code text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_config_repository_access_operat_helper_generation_check CHECK ((installation_generation > 0)),
    CONSTRAINT control_config_repository_access_operatio_revoke_attempts_check CHECK ((revoke_attempts >= 0)),
    CONSTRAINT control_config_repository_access_operations_check CHECK ((((state = 'issued'::text) AND (access_ciphertext IS NOT NULL) AND (expires_at IS NOT NULL)) OR (state <> 'issued'::text))),
    CONSTRAINT control_config_repository_access_operations_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'issued'::text, 'uncertain'::text, 'revoked'::text])))
);


--
-- Name: control_config_repository_lease_authority; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_config_repository_lease_authority (
    repository_id text NOT NULL,
    last_fencing_token bigint DEFAULT 0 NOT NULL,
    lease_id text,
    assignment_id text,
    environment_id text,
    machine_id text,
    installation_generation bigint,
    base_remote_revision text,
    operation_id text,
    acquired_at timestamp with time zone,
    expires_at timestamp with time zone,
    revoked_at timestamp with time zone,
    version bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_config_repository_lease_author_last_fencing_token_check CHECK ((last_fencing_token >= 0)),
    CONSTRAINT control_config_repository_lease_authority_check CHECK ((((lease_id IS NULL) AND (assignment_id IS NULL) AND (environment_id IS NULL) AND (machine_id IS NULL) AND (installation_generation IS NULL) AND (operation_id IS NULL) AND (acquired_at IS NULL) AND (expires_at IS NULL)) OR ((lease_id IS NOT NULL) AND (assignment_id IS NOT NULL) AND (environment_id IS NOT NULL) AND (machine_id IS NOT NULL) AND (installation_generation > 0) AND (operation_id IS NOT NULL) AND (acquired_at IS NOT NULL) AND (expires_at > acquired_at)))),
    CONSTRAINT control_config_repository_lease_authority_version_check CHECK ((version >= 0))
);


--
-- Name: control_config_repository_lease_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_config_repository_lease_operations (
    operation_id text NOT NULL,
    operation_type text NOT NULL,
    request_hash bytea NOT NULL,
    repository_id text NOT NULL,
    lease_id text,
    fencing_token bigint,
    result_state text NOT NULL,
    expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    assignment_id text,
    environment_id text,
    machine_id text,
    base_remote_revision text,
    CONSTRAINT control_config_repository_lease_operations_fencing_token_check CHECK (((fencing_token IS NULL) OR (fencing_token > 0))),
    CONSTRAINT control_config_repository_lease_operations_operation_type_check CHECK ((operation_type = ANY (ARRAY['acquire'::text, 'renew'::text, 'release'::text, 'revoke'::text]))),
    CONSTRAINT control_config_repository_lease_operations_result_state_check CHECK ((result_state = ANY (ARRAY['acquired'::text, 'renewed'::text, 'released'::text, 'revoked'::text, 'busy'::text, 'lost'::text])))
);


--
-- Name: control_config_repository_migration_reviews; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_config_repository_migration_reviews (
    source text NOT NULL,
    source_id text NOT NULL,
    repository_id text,
    reason text NOT NULL,
    details jsonb DEFAULT '{}'::jsonb NOT NULL,
    reviewed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_config_repository_migration_reviews_reason_check CHECK ((reason = ANY (ARRAY['missing_external_repository_id'::text, 'missing_authorization'::text, 'canonical_identity_already_exists'::text, 'incomplete_canonical_connection'::text])))
);


--
-- Name: control_config_sync_migration_reviews; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_config_sync_migration_reviews (
    source text NOT NULL,
    project_id text NOT NULL,
    machine_id text NOT NULL,
    environment_id text,
    reason text NOT NULL,
    details jsonb DEFAULT '{}'::jsonb NOT NULL,
    reviewed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_config_sync_migration_reviews_reason_check CHECK ((reason = ANY (ARRAY['helper_binding_required'::text, 'baseline_verification_required'::text, 'repository_reauthorization_required'::text])))
);


--
-- Name: control_config_sync_status_history; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_config_sync_status_history (
    environment_id text NOT NULL,
    sync_revision bigint NOT NULL,
    repository_id text NOT NULL,
    assignment_id text NOT NULL,
    machine_id text NOT NULL,
    installation_generation bigint NOT NULL,
    state text NOT NULL,
    error_code text,
    remote_revision text,
    observed_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: control_config_sync_statuses; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_config_sync_statuses (
    environment_id text NOT NULL,
    repository_id text NOT NULL,
    assignment_id text NOT NULL,
    machine_id text NOT NULL,
    installation_generation bigint NOT NULL,
    warning_revision text NOT NULL,
    policy_revision text NOT NULL,
    sync_revision bigint NOT NULL,
    state text NOT NULL,
    mode text DEFAULT 'pull_only'::text NOT NULL,
    remote_revision text,
    manifest_health text DEFAULT ''::text NOT NULL,
    manifest_revision text,
    managed_path_count integer DEFAULT 0 NOT NULL,
    pending_clean_path_count integer DEFAULT 0 NOT NULL,
    last_applied_revision text,
    last_published_revision text,
    lease_id text,
    fencing_token bigint,
    skipped jsonb DEFAULT '[]'::jsonb NOT NULL,
    conflicts jsonb DEFAULT '[]'::jsonb NOT NULL,
    error_code text,
    recovery_actions jsonb DEFAULT '[]'::jsonb NOT NULL,
    last_attempt_at timestamp with time zone,
    last_successful_at timestamp with time zone,
    machine_updated_at timestamp with time zone NOT NULL,
    observed_at timestamp with time zone DEFAULT now() NOT NULL,
    review jsonb DEFAULT '[]'::jsonb NOT NULL,
    CONSTRAINT control_config_sync_statuses_check CHECK (((fencing_token IS NULL) OR ((fencing_token > 0) AND (lease_id IS NOT NULL)))),
    CONSTRAINT control_config_sync_statuses_check1 CHECK (((jsonb_typeof(skipped) = 'array'::text) AND (jsonb_typeof(conflicts) = 'array'::text) AND (jsonb_typeof(recovery_actions) = 'array'::text))),
    CONSTRAINT control_config_sync_statuses_helper_generation_check CHECK ((installation_generation > 0)),
    CONSTRAINT control_config_sync_statuses_managed_path_count_check CHECK ((managed_path_count >= 0)),
    CONSTRAINT control_config_sync_statuses_manifest_health_check CHECK ((manifest_health = ANY (ARRAY[''::text, 'healthy'::text, 'empty'::text, 'missing'::text, 'invalid'::text]))),
    CONSTRAINT control_config_sync_statuses_mode_check CHECK ((mode = ANY (ARRAY['pull_only'::text, 'push_only'::text, 'bidirectional'::text]))),
    CONSTRAINT control_config_sync_statuses_pending_clean_path_count_check CHECK ((pending_clean_path_count >= 0)),
    CONSTRAINT control_config_sync_statuses_state_check CHECK ((state = ANY (ARRAY['disabled'::text, 'consent_required'::text, 'restoring'::text, 'watching'::text, 'pending'::text, 'syncing'::text, 'healthy'::text, 'warning'::text, 'conflict'::text, 'offline'::text, 'revoked'::text, 'error'::text, 'sync_uncertain'::text]))),
    CONSTRAINT control_config_sync_statuses_sync_revision_check CHECK ((sync_revision >= 0))
);


--
-- Name: control_connector_generations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_connector_generations (
    environment_id text NOT NULL,
    machine_id text NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    edge_pool text NOT NULL,
    edge_node_id text,
    state text DEFAULT 'pending'::text NOT NULL,
    admission_jti_hash bytea,
    admission_operation_key text,
    admission_request_hash bytea,
    admission_credential_ciphertext bytea,
    expires_at timestamp with time zone,
    revoked_at timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    connector_id text DEFAULT 'runtime'::text NOT NULL,
    CONSTRAINT control_connector_generations_generation_check CHECK ((generation > 0)),
    CONSTRAINT control_connector_generations_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'admitted'::text, 'draining'::text, 'revoked'::text, 'replaced'::text])))
);


--
-- Name: control_environments; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_environments (
    id text NOT NULL,
    workspace_id text NOT NULL,
    owner_user_id text,
    desired_state text DEFAULT 'active'::text NOT NULL,
    desired_version bigint DEFAULT 1 NOT NULL,
    applied_state text DEFAULT 'unknown'::text NOT NULL,
    applied_version bigint DEFAULT 0 NOT NULL,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_environments_applied_state_check CHECK ((applied_state = ANY (ARRAY['unknown'::text, 'pending'::text, 'ready'::text, 'degraded'::text, 'suspended'::text, 'revoked'::text, 'deleted'::text]))),
    CONSTRAINT control_environments_check CHECK (((desired_version > 0) AND (applied_version >= 0))),
    CONSTRAINT control_environments_desired_state_check CHECK ((desired_state = ANY (ARRAY['active'::text, 'suspended'::text, 'revoked'::text, 'deleting'::text])))
);


--
-- Name: control_helper_enrollments; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_helper_enrollments (
    id text NOT NULL,
    environment_id text NOT NULL,
    helper_id text NOT NULL,
    jti_hash bytea NOT NULL,
    operation_key text NOT NULL,
    request_hash bytea NOT NULL,
    grant_ciphertext bytea NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_helper_enrollments_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'consumed'::text, 'expired'::text, 'revoked'::text])))
);


--
-- Name: control_helpers; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_helpers (
    id text NOT NULL,
    environment_id text NOT NULL,
    key_thumbprint text,
    public_key bytea,
    state text DEFAULT 'pending'::text NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    replacement_operation_key text,
    replacement_connector_generation bigint,
    last_seen_at timestamp with time zone,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_helpers_generation_check CHECK ((generation > 0)),
    CONSTRAINT control_helpers_public_key_check CHECK (((public_key IS NULL) OR (octet_length(public_key) = 32))),
    CONSTRAINT control_helpers_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'active'::text, 'replaced'::text, 'revoked'::text, 'deleted'::text])))
);


--
-- Name: control_operation_recoveries; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_operation_recoveries (
    operation_key text NOT NULL,
    operation_id text NOT NULL,
    actor_user_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_operation_recoveries_operation_key_check CHECK (((length(TRIM(BOTH FROM operation_key)) >= 1) AND (length(TRIM(BOTH FROM operation_key)) <= 256)))
);


--
-- Name: control_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_operations (
    id text NOT NULL,
    operation_key text NOT NULL,
    operation_type text NOT NULL,
    request_hash bytea NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    result jsonb DEFAULT '{}'::jsonb NOT NULL,
    last_error text,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone,
    lease_expires_at timestamp with time zone,
    uncertain_at timestamp with time zone,
    completed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_operations_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT control_operations_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'uncertain'::text, 'dead_letter'::text])))
);


--
-- Name: control_reconciliation_attempts; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_reconciliation_attempts (
    id text NOT NULL,
    environment_id text NOT NULL,
    desired_version bigint NOT NULL,
    state text DEFAULT 'started'::text NOT NULL,
    operation_id text,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    finished_at timestamp with time zone,
    last_error text,
    CONSTRAINT control_reconciliation_attempts_state_check CHECK ((state = ANY (ARRAY['started'::text, 'succeeded'::text, 'failed'::text, 'cancelled'::text, 'uncertain'::text])))
);


--
-- Name: control_route_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_route_operations (
    operation_key text NOT NULL,
    operation_type text NOT NULL,
    request_hash bytea NOT NULL,
    route_id text NOT NULL,
    result_revision bigint,
    result jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_route_operations_operation_key_check CHECK (((length(TRIM(BOTH FROM operation_key)) >= 1) AND (length(TRIM(BOTH FROM operation_key)) <= 256))),
    CONSTRAINT control_route_operations_operation_type_check CHECK ((operation_type = ANY (ARRAY['create'::text, 'transition'::text])))
);


--
-- Name: control_routes; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_routes (
    id text NOT NULL,
    environment_id text NOT NULL,
    kind text NOT NULL,
    public_host text NOT NULL,
    target_host text NOT NULL,
    target_port integer NOT NULL,
    desired_revision bigint DEFAULT 1 NOT NULL,
    desired_state text DEFAULT 'attached'::text NOT NULL,
    applied_revision bigint DEFAULT 0 NOT NULL,
    applied_node_id text,
    applied_generation bigint,
    drain_deadline timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    connector_id text DEFAULT 'runtime'::text NOT NULL,
    CONSTRAINT control_routes_desired_revision_check CHECK ((desired_revision > 0)),
    CONSTRAINT control_routes_desired_state_check CHECK ((desired_state = ANY (ARRAY['attached'::text, 'detaching'::text, 'detached'::text, 'replacing'::text]))),
    CONSTRAINT control_routes_kind_check CHECK ((kind = ANY (ARRAY['runtime_https_wss'::text, 'preview_public_https_wss'::text]))),
    CONSTRAINT control_routes_target_host_check CHECK ((target_host = ANY (ARRAY['127.0.0.1'::text, '::1'::text]))),
    CONSTRAINT control_routes_target_port_check CHECK (((target_port >= 1) AND (target_port <= 65535)))
);


--
-- Name: control_signing_key_revocation_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_signing_key_revocation_operations (
    operation_key text NOT NULL,
    key_id text NOT NULL,
    reason text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_signing_key_revocation_operations_operation_key_check CHECK (((length(TRIM(BOTH FROM operation_key)) >= 1) AND (length(TRIM(BOTH FROM operation_key)) <= 256)))
);


--
-- Name: control_signing_key_revocations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_signing_key_revocations (
    key_id text NOT NULL,
    reason text NOT NULL,
    revoked_at timestamp with time zone NOT NULL,
    actor_user_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_signing_key_revocations_key_id_check CHECK (((length(TRIM(BOTH FROM key_id)) >= 1) AND (length(TRIM(BOTH FROM key_id)) <= 128))),
    CONSTRAINT control_signing_key_revocations_reason_check CHECK (((length(TRIM(BOTH FROM reason)) >= 1) AND (length(TRIM(BOTH FROM reason)) <= 512)))
);


--
-- Name: control_sync_leases; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_sync_leases (
    machine_id text,
    address_slot integer NOT NULL,
    tags jsonb DEFAULT '[]'::jsonb NOT NULL,
    CONSTRAINT control_sync_leases_address_slot_check CHECK ((((address_slot >= 2) AND (address_slot <= 65534)) AND (((address_slot % 256) >= 1) AND ((address_slot % 256) <= 254)))),
    CONSTRAINT control_sync_leases_tags_check CHECK (((jsonb_typeof(tags) = 'array'::text) AND (jsonb_array_length(tags) <= 32)))
);


--
-- Name: control_tunnel_nodes; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_tunnel_nodes (
    id text NOT NULL,
    edge_pool text NOT NULL,
    protocol_version text NOT NULL,
    process_epoch text NOT NULL,
    endpoint_host text,
    endpoint_tcp_port integer,
    endpoint_quic_port integer,
    state text DEFAULT 'registered'::text NOT NULL,
    ready boolean DEFAULT false NOT NULL,
    capacity jsonb DEFAULT '{}'::jsonb NOT NULL,
    observation jsonb DEFAULT '{}'::jsonb NOT NULL,
    last_heartbeat_at timestamp with time zone,
    drain_deadline timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    carrier_endpoint_host text,
    carrier_endpoint_tcp_port integer,
    carrier_endpoint_quic_port integer,
    carrier_server_spki_sha256 text,
    carrier_server_certificate_chain_pem text,
    node_generation bigint DEFAULT 1 NOT NULL,
    region text,
    failure_domain text,
    roles text[] DEFAULT ARRAY[]::text[] NOT NULL,
    transports text[] DEFAULT ARRAY[]::text[] NOT NULL,
    capacity_limit bigint DEFAULT 0 NOT NULL,
    capacity_used bigint DEFAULT 0 NOT NULL,
    capacity_observed_at timestamp with time zone,
    registry_expires_at timestamp with time zone,
    allowed_account_ids text[],
    peer_relay_wireguard_public_key bytea,
    peer_relay_disco_public_key bytea,
    peer_relay_virtual_address inet,
    public_ingress_ipv4 inet,
    public_ingress_ipv6 inet,
    public_ingress_verified_at timestamp with time zone,
    CONSTRAINT control_tunnel_nodes_capacity_limit_check CHECK ((capacity_limit >= 0)),
    CONSTRAINT control_tunnel_nodes_capacity_used_check CHECK ((capacity_used >= 0)),
    CONSTRAINT control_tunnel_nodes_carrier_endpoint_check CHECK ((((carrier_endpoint_host IS NULL) AND (carrier_endpoint_tcp_port IS NULL) AND (carrier_endpoint_quic_port IS NULL)) OR (((length(TRIM(BOTH FROM carrier_endpoint_host)) >= 1) AND (length(TRIM(BOTH FROM carrier_endpoint_host)) <= 253)) AND ((carrier_endpoint_tcp_port >= 1) AND (carrier_endpoint_tcp_port <= 65535)) AND ((carrier_endpoint_quic_port >= 1) AND (carrier_endpoint_quic_port <= 65535)) AND (carrier_endpoint_tcp_port <> carrier_endpoint_quic_port)))),
    CONSTRAINT control_tunnel_nodes_carrier_server_certificate_chain_pem_check CHECK (((carrier_server_certificate_chain_pem IS NULL) OR ((octet_length(carrier_server_certificate_chain_pem) >= 1) AND (octet_length(carrier_server_certificate_chain_pem) <= 65536)))),
    CONSTRAINT control_tunnel_nodes_carrier_server_spki_sha256_check CHECK (((carrier_server_spki_sha256 IS NULL) OR (carrier_server_spki_sha256 ~ '^sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT control_tunnel_nodes_endpoint_check CHECK ((((endpoint_host IS NULL) AND (endpoint_tcp_port IS NULL) AND (endpoint_quic_port IS NULL)) OR (((length(TRIM(BOTH FROM endpoint_host)) >= 1) AND (length(TRIM(BOTH FROM endpoint_host)) <= 253)) AND ((endpoint_tcp_port >= 1) AND (endpoint_tcp_port <= 65535)) AND ((endpoint_quic_port >= 1) AND (endpoint_quic_port <= 65535))))),
    CONSTRAINT control_tunnel_nodes_node_generation_check CHECK ((node_generation > 0)),
    CONSTRAINT control_tunnel_nodes_peer_relay_identity_check CHECK ((((peer_relay_wireguard_public_key IS NULL) AND (peer_relay_disco_public_key IS NULL) AND (peer_relay_virtual_address IS NULL)) OR ((octet_length(peer_relay_wireguard_public_key) = 32) AND (octet_length(peer_relay_disco_public_key) = 32) AND (family(peer_relay_virtual_address) = 6) AND (peer_relay_virtual_address << 'fd7a:115c:a1e0::/48'::inet) AND (roles @> ARRAY['relay'::text, 'peer_relay'::text]) AND (transports @> ARRAY['derp_quic'::text, 'peer_relay_udp'::text])))),
    CONSTRAINT control_tunnel_nodes_public_address_family CHECK ((((public_ingress_ipv4 IS NULL) OR (family(public_ingress_ipv4) = 4)) AND ((public_ingress_ipv6 IS NULL) OR (family(public_ingress_ipv6) = 6)))),
    CONSTRAINT control_tunnel_nodes_regional_registry_check CHECK ((((region IS NULL) AND (failure_domain IS NULL) AND (cardinality(roles) = 0) AND (cardinality(transports) = 0) AND (registry_expires_at IS NULL)) OR (((length(region) >= 1) AND (length(region) <= 63)) AND ((length(failure_domain) >= 1) AND (length(failure_domain) <= 128)) AND ((cardinality(roles) >= 1) AND (cardinality(roles) <= 2)) AND (roles <@ ARRAY['edge'::text, 'relay'::text, 'peer_relay'::text]) AND ((cardinality(transports) >= 1) AND (cardinality(transports) <= 4)) AND (transports <@ ARRAY['http3'::text, 'http2'::text, 'derp_quic'::text, 'derp_wss'::text, 'peer_relay_udp'::text]) AND (capacity_limit > 0) AND (capacity_used <= capacity_limit) AND (capacity_observed_at IS NOT NULL) AND (registry_expires_at IS NOT NULL)))),
    CONSTRAINT control_tunnel_nodes_state_check CHECK ((state = ANY (ARRAY['registered'::text, 'ready'::text, 'draining'::text, 'offline'::text, 'retired'::text])))
);


--
-- Name: control_usage_counters; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_usage_counters (
    edge_node_id text NOT NULL,
    counter_epoch text NOT NULL,
    environment_id text NOT NULL,
    route_id text NOT NULL,
    route_revision bigint NOT NULL,
    direction text NOT NULL,
    bytes bigint DEFAULT 0 NOT NULL,
    observed_at timestamp with time zone DEFAULT now() NOT NULL,
    authority_id text DEFAULT ''::text NOT NULL,
    CONSTRAINT control_usage_counters_bytes_check CHECK ((bytes >= 0)),
    CONSTRAINT control_usage_counters_direction_check CHECK ((direction = ANY (ARRAY['ingress'::text, 'egress'::text])))
);


--
-- Name: control_usage_key_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_usage_key_operations (
    operation_key text NOT NULL,
    action text NOT NULL,
    key_id text NOT NULL,
    request_hash bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_usage_key_operations_action_check CHECK ((action = ANY (ARRAY['provision'::text, 'revoke'::text]))),
    CONSTRAINT control_usage_key_operations_key_id_check CHECK (((length(TRIM(BOTH FROM key_id)) >= 1) AND (length(TRIM(BOTH FROM key_id)) <= 128))),
    CONSTRAINT control_usage_key_operations_operation_key_check CHECK (((length(TRIM(BOTH FROM operation_key)) >= 1) AND (length(TRIM(BOTH FROM operation_key)) <= 256))),
    CONSTRAINT control_usage_key_operations_request_hash_check CHECK ((octet_length(request_hash) = 32))
);


--
-- Name: control_usage_receipts; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_usage_receipts (
    operation_id text NOT NULL,
    edge_node_id text NOT NULL,
    counter_epoch text NOT NULL,
    environment_id text NOT NULL,
    route_id text NOT NULL,
    route_revision bigint NOT NULL,
    direction text NOT NULL,
    observed_bytes bigint NOT NULL,
    delta_bytes bigint NOT NULL,
    interval_start timestamp with time zone NOT NULL,
    interval_end timestamp with time zone NOT NULL,
    acknowledged_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    authority_id text DEFAULT ''::text NOT NULL,
    workspace_id text,
    disposition text DEFAULT 'accounted'::text NOT NULL,
    quota_exhausted boolean DEFAULT false NOT NULL,
    CONSTRAINT control_usage_receipts_check CHECK ((interval_end >= interval_start)),
    CONSTRAINT control_usage_receipts_delta_bytes_check CHECK ((delta_bytes >= 0)),
    CONSTRAINT control_usage_receipts_direction_check CHECK ((direction = ANY (ARRAY['ingress'::text, 'egress'::text]))),
    CONSTRAINT control_usage_receipts_disposition_check CHECK ((disposition = ANY (ARRAY['accounted'::text, 'expired_not_billed'::text]))),
    CONSTRAINT control_usage_receipts_observed_bytes_check CHECK ((observed_bytes >= 0))
);


--
-- Name: control_usage_verification_keys; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.control_usage_verification_keys (
    key_id text NOT NULL,
    edge_node_id text NOT NULL,
    public_key bytea NOT NULL,
    not_before timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_usage_verification_keys_check CHECK ((expires_at > not_before)),
    CONSTRAINT control_usage_verification_keys_public_key_check CHECK ((octet_length(public_key) = 32))
);


--
-- Name: device_grants; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.device_grants (
    id text NOT NULL,
    client_id text NOT NULL,
    client_label text NOT NULL,
    device_type text NOT NULL,
    os text NOT NULL,
    scopes text[] NOT NULL,
    device_code_hash text NOT NULL,
    user_code_hash text NOT NULL,
    state text NOT NULL,
    user_id text,
    issued_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    poll_interval_seconds integer NOT NULL,
    next_poll_at timestamp with time zone NOT NULL,
    approved_at timestamp with time zone,
    denied_at timestamp with time zone,
    consumed_at timestamp with time zone,
    created_network_hash text NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    CONSTRAINT device_grants_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'approved'::text, 'denied'::text, 'consumed'::text, 'expired'::text])))
);


--
-- Name: device_service_discovery_policies; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.device_service_discovery_policies (
    machine_id text NOT NULL,
    generation bigint NOT NULL,
    automatic_discovery_enabled boolean NOT NULL,
    explicit_ports jsonb NOT NULL,
    CONSTRAINT device_service_discovery_policies_explicit_ports_check CHECK (((jsonb_typeof(explicit_ports) = 'array'::text) AND (jsonb_array_length(explicit_ports) <= 64))),
    CONSTRAINT device_service_discovery_policies_generation_check CHECK (((generation >= 1) AND (generation <= '9007199254740991'::bigint)))
);


--
-- Name: device_service_snapshots; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.device_service_snapshots (
    machine_id text NOT NULL,
    installation_generation bigint NOT NULL,
    boot_id text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    generation bigint NOT NULL,
    services jsonb NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT device_service_snapshots_boot_id_check CHECK (((length(boot_id) >= 16) AND (length(boot_id) <= 64))),
    CONSTRAINT device_service_snapshots_generation_check CHECK (((generation >= 1) AND (generation <= '9007199254740991'::bigint))),
    CONSTRAINT device_service_snapshots_installation_generation_check CHECK ((installation_generation > 0)),
    CONSTRAINT device_service_snapshots_services_check CHECK (((jsonb_typeof(services) = 'array'::text) AND (jsonb_array_length(services) <= 64)))
);


--
-- Name: diagnostic_upload_intents; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.diagnostic_upload_intents (
    id text NOT NULL,
    user_id text NOT NULL,
    cli_client_session_id text NOT NULL,
    operation_key text NOT NULL,
    request_hash bytea NOT NULL,
    correlation_id text NOT NULL,
    object_key text NOT NULL,
    expected_bytes bigint NOT NULL,
    sha256 bytea NOT NULL,
    categories text[] NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    retain_until timestamp with time zone NOT NULL,
    created_at timestamp with time zone NOT NULL,
    uploaded_at timestamp with time zone,
    object_etag text,
    CONSTRAINT diagnostic_upload_intents_categories_check CHECK ((((cardinality(categories) >= 1) AND (cardinality(categories) <= 16)) AND (categories <@ ARRAY['manifest'::text, 'recent_events'::text, 'redacted_events'::text, 'status'::text]))),
    CONSTRAINT diagnostic_upload_intents_check CHECK ((expires_at > created_at)),
    CONSTRAINT diagnostic_upload_intents_check1 CHECK ((retain_until >= expires_at)),
    CONSTRAINT diagnostic_upload_intents_check2 CHECK (((state = 'uploaded'::text) = (uploaded_at IS NOT NULL))),
    CONSTRAINT diagnostic_upload_intents_check3 CHECK (((state = 'uploaded'::text) = (object_etag IS NOT NULL))),
    CONSTRAINT diagnostic_upload_intents_check4 CHECK (((uploaded_at IS NULL) OR ((uploaded_at >= created_at) AND (uploaded_at <= retain_until)))),
    CONSTRAINT diagnostic_upload_intents_correlation_id_check CHECK ((correlation_id ~ '^pb-[a-f0-9]{32}$'::text)),
    CONSTRAINT diagnostic_upload_intents_expected_bytes_check CHECK (((expected_bytes >= 1) AND (expected_bytes <= 26214400))),
    CONSTRAINT diagnostic_upload_intents_id_check CHECK ((id ~ '^diag_[A-Za-z0-9_-]{16,128}$'::text)),
    CONSTRAINT diagnostic_upload_intents_object_etag_check CHECK (((object_etag IS NULL) OR ((octet_length(object_etag) >= 1) AND (octet_length(object_etag) <= 256)))),
    CONSTRAINT diagnostic_upload_intents_object_key_check CHECK ((object_key ~ '^diagnostics/[A-Za-z0-9_-]{16,128}/[A-Za-z0-9_-]{16,128}\.zip$'::text)),
    CONSTRAINT diagnostic_upload_intents_operation_key_check CHECK (((octet_length(operation_key) >= 16) AND (octet_length(operation_key) <= 200))),
    CONSTRAINT diagnostic_upload_intents_request_hash_check CHECK ((octet_length(request_hash) = 32)),
    CONSTRAINT diagnostic_upload_intents_sha256_check CHECK ((octet_length(sha256) = 32)),
    CONSTRAINT diagnostic_upload_intents_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'uploaded'::text, 'expired'::text])))
);


--
-- Name: edge_dns_provider_budgets; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.edge_dns_provider_budgets (
    zone_id text NOT NULL,
    tokens double precision NOT NULL,
    refilled_at timestamp with time zone NOT NULL,
    blocked_until timestamp with time zone NOT NULL,
    CONSTRAINT edge_dns_provider_budgets_tokens_check CHECK (((tokens >= (0)::double precision) AND (tokens <= (5)::double precision)))
);


--
-- Name: edge_dns_publications; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.edge_dns_publications (
    hostname text NOT NULL,
    owner_id text NOT NULL,
    resource_generation bigint NOT NULL,
    readiness_version text NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    valid_until timestamp with time zone,
    publication_generation bigint NOT NULL,
    desired_addresses jsonb DEFAULT '[]'::jsonb NOT NULL,
    provider_records jsonb DEFAULT '[]'::jsonb NOT NULL,
    state text NOT NULL,
    failure_code text,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    verified_at timestamp with time zone,
    CONSTRAINT edge_dns_publications_desired_addresses_check CHECK ((jsonb_typeof(desired_addresses) = 'array'::text)),
    CONSTRAINT edge_dns_publications_provider_records_check CHECK ((jsonb_typeof(provider_records) = 'array'::text)),
    CONSTRAINT edge_dns_publications_publication_generation_check CHECK ((publication_generation > 0)),
    CONSTRAINT edge_dns_publications_resource_generation_check CHECK ((resource_generation > 0)),
    CONSTRAINT edge_dns_publications_state_check CHECK ((state = ANY (ARRAY['desired'::text, 'submitted'::text, 'verified'::text, 'failed'::text])))
);


--
-- Name: environment_authorities; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_authorities (
    account_id text NOT NULL,
    generation bigint NOT NULL,
    authority_id text NOT NULL,
    previous_authority_id text,
    operation_id text NOT NULL,
    envelope bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT environment_authorities_authority_id_check CHECK ((authority_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_authorities_envelope_check CHECK (((octet_length(envelope) >= 1) AND (octet_length(envelope) <= 2097152))),
    CONSTRAINT environment_authorities_generation_check CHECK (((generation >= 1) AND (generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_authorities_operation_id_check CHECK ((operation_id ~ '^envop_[0-9a-f]{32}$'::text)),
    CONSTRAINT environment_authorities_previous_authority_id_check CHECK (((previous_authority_id IS NULL) OR (previous_authority_id ~ '^sha256:[0-9a-f]{64}$'::text)))
);


--
-- Name: environment_authority_heads; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_authority_heads (
    account_id text NOT NULL,
    generation bigint NOT NULL,
    authority_id text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT environment_authority_heads_authority_id_check CHECK ((authority_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_authority_heads_generation_check CHECK (((generation >= 1) AND (generation <= '9007199254740991'::bigint)))
);


--
-- Name: environment_authority_roots; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_authority_roots (
    account_id text NOT NULL,
    key_id text NOT NULL,
    public_key bytea NOT NULL,
    CONSTRAINT environment_authority_roots_key_id_check CHECK ((key_id ~ '^aek_[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_authority_roots_public_key_check CHECK ((octet_length(public_key) = 32))
);


--
-- Name: environment_authority_transitions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_authority_transitions (
    transition_id text NOT NULL,
    account_id text NOT NULL,
    operation_id text NOT NULL,
    base_authority_id text,
    proposed_generation bigint NOT NULL,
    proposed_authority_id text NOT NULL,
    proposed_authority bytea NOT NULL,
    state text NOT NULL,
    required_scopes text[] NOT NULL,
    abort_operation_id text,
    abort_authorization bytea,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    activated_at timestamp with time zone,
    aborted_at timestamp with time zone,
    CONSTRAINT environment_authority_transitions_abort_operation_id_check CHECK (((abort_operation_id IS NULL) OR (abort_operation_id ~ '^envop_[0-9a-f]{32}$'::text))),
    CONSTRAINT environment_authority_transitions_operation_id_check CHECK ((operation_id ~ '^envop_[0-9a-f]{32}$'::text)),
    CONSTRAINT environment_authority_transitions_proposed_authority_check CHECK (((octet_length(proposed_authority) >= 1) AND (octet_length(proposed_authority) <= 2097152))),
    CONSTRAINT environment_authority_transitions_proposed_authority_id_check CHECK ((proposed_authority_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_authority_transitions_proposed_generation_check CHECK (((proposed_generation >= 1) AND (proposed_generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_authority_transitions_state_check CHECK ((state = ANY (ARRAY['staged'::text, 'ready'::text, 'active'::text, 'aborted'::text]))),
    CONSTRAINT environment_authority_transitions_transition_id_check CHECK ((transition_id ~ '^sha256:[0-9a-f]{64}$'::text))
);


--
-- Name: environment_host_bootstraps; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_host_bootstraps (
    account_id text NOT NULL,
    machine_id text NOT NULL,
    subject_generation bigint NOT NULL,
    key_generation bigint NOT NULL,
    recipient_key_id text NOT NULL,
    authority_generation bigint NOT NULL,
    authority_id text NOT NULL,
    global_version bigint NOT NULL,
    global_key_epoch bigint NOT NULL,
    global_manifest_id text NOT NULL,
    global_envelope bytea NOT NULL,
    machine_version bigint NOT NULL,
    machine_key_epoch bigint NOT NULL,
    machine_manifest_id text NOT NULL,
    machine_envelope bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT environment_host_bootstraps_authority_generation_check CHECK (((authority_generation >= 1) AND (authority_generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_host_bootstraps_authority_id_check CHECK ((authority_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_host_bootstraps_global_envelope_check CHECK (((octet_length(global_envelope) >= 1) AND (octet_length(global_envelope) <= 1048576))),
    CONSTRAINT environment_host_bootstraps_key_generation_check CHECK (((key_generation >= 1) AND (key_generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_host_bootstraps_machine_envelope_check CHECK (((octet_length(machine_envelope) >= 1) AND (octet_length(machine_envelope) <= 1048576))),
    CONSTRAINT environment_host_bootstraps_recipient_key_id_check CHECK ((recipient_key_id ~ '^envk_[A-Za-z0-9_-]{43}$'::text)),
    CONSTRAINT environment_host_bootstraps_subject_generation_check CHECK (((subject_generation >= 1) AND (subject_generation <= '9007199254740991'::bigint)))
);


--
-- Name: environment_key_bindings; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_key_bindings (
    binding_id text NOT NULL,
    account_id text NOT NULL,
    subject_kind text NOT NULL,
    subject_id text NOT NULL,
    subject_generation bigint NOT NULL,
    key_generation bigint NOT NULL,
    signing_key_id text,
    signing_public_key bytea,
    recipient_key_id text NOT NULL,
    recipient_public_key bytea NOT NULL,
    envelope bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT environment_key_bindings_binding_id_check CHECK ((binding_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_key_bindings_envelope_check CHECK (((octet_length(envelope) >= 1) AND (octet_length(envelope) <= 2048))),
    CONSTRAINT environment_key_bindings_key_generation_check CHECK (((key_generation >= 1) AND (key_generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_key_bindings_recipient_key_id_check CHECK ((recipient_key_id ~ '^envk_[A-Za-z0-9_-]{43}$'::text)),
    CONSTRAINT environment_key_bindings_recipient_public_key_check CHECK ((octet_length(recipient_public_key) = 32)),
    CONSTRAINT environment_key_bindings_signing_key_id_check CHECK (((signing_key_id IS NULL) OR (signing_key_id ~ '^sigk_[A-Za-z0-9_-]{43}$'::text))),
    CONSTRAINT environment_key_bindings_signing_public_key_check CHECK (((signing_public_key IS NULL) OR (octet_length(signing_public_key) = 32))),
    CONSTRAINT environment_key_bindings_subject_generation_check CHECK (((subject_generation >= 1) AND (subject_generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_key_bindings_subject_kind_check CHECK ((subject_kind = ANY (ARRAY['manager_cli'::text, 'manager_browser'::text, 'host'::text, 'recovery'::text])))
);


--
-- Name: environment_key_enrollment_requests; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_key_enrollment_requests (
    id text NOT NULL,
    account_id text NOT NULL,
    requester_kind text NOT NULL,
    requester_id text NOT NULL,
    subject_kind text NOT NULL,
    subject_id text NOT NULL,
    subject_generation bigint NOT NULL,
    key_generation bigint NOT NULL,
    operation_id text NOT NULL,
    request_digest bytea NOT NULL,
    canonical_request bytea NOT NULL,
    signing_proof bytea,
    recipient_key_id text NOT NULL,
    recipient_public_key bytea NOT NULL,
    safety_code text NOT NULL,
    challenge_envelope bytea NOT NULL,
    expected_proof bytea NOT NULL,
    state text NOT NULL,
    transition_id text,
    expires_at timestamp with time zone NOT NULL,
    proved_at timestamp with time zone,
    approved_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT environment_key_enrollment_requests_canonical_request_check CHECK (((octet_length(canonical_request) >= 1) AND (octet_length(canonical_request) <= 8192))),
    CONSTRAINT environment_key_enrollment_requests_challenge_envelope_check CHECK (((octet_length(challenge_envelope) >= 49) AND (octet_length(challenge_envelope) <= 256))),
    CONSTRAINT environment_key_enrollment_requests_expected_proof_check CHECK ((octet_length(expected_proof) = 32)),
    CONSTRAINT environment_key_enrollment_requests_key_generation_check CHECK (((key_generation >= 1) AND (key_generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_key_enrollment_requests_operation_id_check CHECK ((operation_id ~ '^envop_[0-9a-f]{32}$'::text)),
    CONSTRAINT environment_key_enrollment_requests_recipient_key_id_check CHECK ((recipient_key_id ~ '^envk_[A-Za-z0-9_-]{43}$'::text)),
    CONSTRAINT environment_key_enrollment_requests_recipient_public_key_check CHECK ((octet_length(recipient_public_key) = 32)),
    CONSTRAINT environment_key_enrollment_requests_request_digest_check CHECK ((octet_length(request_digest) = 32)),
    CONSTRAINT environment_key_enrollment_requests_requester_kind_check CHECK ((requester_kind = ANY (ARRAY['human_session'::text, 'cli_session'::text, 'machine'::text]))),
    CONSTRAINT environment_key_enrollment_requests_safety_code_check CHECK ((safety_code ~ '^[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}$'::text)),
    CONSTRAINT environment_key_enrollment_requests_signing_proof_check CHECK (((signing_proof IS NULL) OR (octet_length(signing_proof) = 64))),
    CONSTRAINT environment_key_enrollment_requests_state_check CHECK ((state = ANY (ARRAY['challenge'::text, 'pending'::text, 'approved'::text, 'expired'::text]))),
    CONSTRAINT environment_key_enrollment_requests_subject_generation_check CHECK (((subject_generation >= 1) AND (subject_generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_key_enrollment_requests_subject_kind_check CHECK ((subject_kind = ANY (ARRAY['manager_cli'::text, 'manager_browser'::text, 'host'::text])))
);


--
-- Name: environment_observations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_observations (
    machine_id text NOT NULL,
    account_id text NOT NULL,
    host_recipient_key_id text NOT NULL,
    observation_seq bigint NOT NULL,
    authority_generation bigint,
    authority_id text,
    global_version bigint,
    global_key_epoch bigint,
    global_manifest_id text,
    machine_version bigint,
    machine_key_epoch bigint,
    machine_manifest_id text,
    state text NOT NULL,
    error_code text,
    observed_at timestamp with time zone NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT environment_observations_check CHECK ((((state = 'failed'::text) AND (error_code IS NOT NULL)) OR ((state <> 'failed'::text) AND (error_code IS NULL)))),
    CONSTRAINT environment_observations_check1 CHECK (((authority_generation IS NULL) = (authority_id IS NULL))),
    CONSTRAINT environment_observations_check2 CHECK ((((global_version IS NULL) = (global_key_epoch IS NULL)) AND ((global_version IS NULL) = (global_manifest_id IS NULL)))),
    CONSTRAINT environment_observations_check3 CHECK ((((machine_version IS NULL) = (machine_key_epoch IS NULL)) AND ((machine_version IS NULL) = (machine_manifest_id IS NULL)))),
    CONSTRAINT environment_observations_error_code_check CHECK (((error_code IS NULL) OR (error_code ~ '^[a-z0-9_]{1,64}$'::text))),
    CONSTRAINT environment_observations_host_recipient_key_id_check CHECK ((host_recipient_key_id ~ '^envk_[A-Za-z0-9_-]{43}$'::text)),
    CONSTRAINT environment_observations_observation_seq_check CHECK ((observation_seq > 0)),
    CONSTRAINT environment_observations_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'applied'::text, 'failed'::text])))
);


--
-- Name: environment_password_vaults; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_password_vaults (
    account_id text NOT NULL,
    generation bigint NOT NULL,
    document_id text NOT NULL,
    envelope bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT environment_password_vaults_document_id_check CHECK ((document_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_password_vaults_envelope_check CHECK (((octet_length(envelope) >= 1) AND (octet_length(envelope) <= 264192))),
    CONSTRAINT environment_password_vaults_generation_check CHECK (((generation >= 1) AND (generation <= '9007199254740991'::bigint)))
);


--
-- Name: environment_scope_manifests; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_scope_manifests (
    scope_id text NOT NULL,
    version bigint NOT NULL,
    key_epoch bigint NOT NULL,
    authority_generation bigint NOT NULL,
    authority_id text NOT NULL,
    operation_id text NOT NULL,
    manifest_id text NOT NULL,
    envelope bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT environment_scope_manifests_authority_generation_check CHECK (((authority_generation >= 1) AND (authority_generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_scope_manifests_authority_id_check CHECK ((authority_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_scope_manifests_envelope_check CHECK (((octet_length(envelope) >= 1) AND (octet_length(envelope) <= 1048576))),
    CONSTRAINT environment_scope_manifests_key_epoch_check CHECK (((key_epoch >= 1) AND (key_epoch <= '9007199254740991'::bigint))),
    CONSTRAINT environment_scope_manifests_manifest_id_check CHECK ((manifest_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_scope_manifests_operation_id_check CHECK ((operation_id ~ '^envop_[0-9a-f]{32}$'::text)),
    CONSTRAINT environment_scope_manifests_version_check CHECK (((version >= 1) AND (version <= '9007199254740991'::bigint)))
);


--
-- Name: environment_scope_names; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_scope_names (
    scope_id text NOT NULL,
    name text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT environment_scope_names_name_check CHECK (((octet_length(name) >= 1) AND (octet_length(name) <= 128)))
);


--
-- Name: environment_scopes; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_scopes (
    id text NOT NULL,
    account_id text NOT NULL,
    scope text NOT NULL,
    machine_id text,
    scope_state text NOT NULL,
    version bigint NOT NULL,
    key_epoch bigint NOT NULL,
    authority_generation bigint NOT NULL,
    authority_id text NOT NULL,
    manifest_id text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT environment_scopes_authority_generation_check CHECK (((authority_generation >= 1) AND (authority_generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_scopes_authority_id_check CHECK ((authority_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_scopes_check CHECK ((((scope = 'global'::text) AND (machine_id IS NULL) AND (scope_state = 'active'::text)) OR ((scope = 'machine'::text) AND (machine_id IS NOT NULL)))),
    CONSTRAINT environment_scopes_key_epoch_check CHECK (((key_epoch >= 1) AND (key_epoch <= '9007199254740991'::bigint))),
    CONSTRAINT environment_scopes_manifest_id_check CHECK ((manifest_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_scopes_scope_check CHECK ((scope = ANY (ARRAY['global'::text, 'machine'::text]))),
    CONSTRAINT environment_scopes_scope_state_check CHECK ((scope_state = ANY (ARRAY['active'::text, 'retired'::text]))),
    CONSTRAINT environment_scopes_version_check CHECK (((version >= 1) AND (version <= '9007199254740991'::bigint)))
);


--
-- Name: environment_transition_manifests; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_transition_manifests (
    transition_id text NOT NULL,
    scope_ref text NOT NULL,
    expected_version bigint NOT NULL,
    version bigint NOT NULL,
    key_epoch bigint NOT NULL,
    operation_id text NOT NULL,
    manifest_id text NOT NULL,
    envelope bytea NOT NULL,
    names text[] NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT environment_transition_manifests_envelope_check CHECK (((octet_length(envelope) >= 1) AND (octet_length(envelope) <= 1048576))),
    CONSTRAINT environment_transition_manifests_expected_version_check CHECK (((expected_version >= 0) AND (expected_version <= '9007199254740990'::bigint))),
    CONSTRAINT environment_transition_manifests_key_epoch_check CHECK (((key_epoch >= 1) AND (key_epoch <= '9007199254740991'::bigint))),
    CONSTRAINT environment_transition_manifests_manifest_id_check CHECK ((manifest_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_transition_manifests_operation_id_check CHECK ((operation_id ~ '^envop_[0-9a-f]{32}$'::text)),
    CONSTRAINT environment_transition_manifests_version_check CHECK (((version >= 1) AND (version <= '9007199254740991'::bigint)))
);


--
-- Name: environment_vault_hosts; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_vault_hosts (
    machine_id text NOT NULL,
    account_id text NOT NULL,
    installation_generation bigint NOT NULL,
    host_key_generation bigint NOT NULL,
    host_public bytea NOT NULL,
    writer_public bytea NOT NULL,
    selection_generation bigint DEFAULT 0 NOT NULL,
    selection bytea DEFAULT '\x5b5d'::bytea NOT NULL,
    projection_revision bigint DEFAULT 0 NOT NULL,
    document_id text DEFAULT ''::text NOT NULL,
    envelope bytea DEFAULT '\x'::bytea NOT NULL,
    applied_fence_generation bigint DEFAULT 0 NOT NULL,
    observation_seq bigint DEFAULT 0 NOT NULL,
    observation_digest bytea,
    observation bytea,
    CONSTRAINT environment_vault_hosts_applied_fence_generation_check CHECK (((applied_fence_generation >= 0) AND (applied_fence_generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_vault_hosts_document_id_check CHECK (((document_id = ''::text) OR (document_id ~ '^sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT environment_vault_hosts_envelope_check CHECK ((octet_length(envelope) <= 327680)),
    CONSTRAINT environment_vault_hosts_host_key_generation_check CHECK (((host_key_generation >= 1) AND (host_key_generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_vault_hosts_host_public_check CHECK ((octet_length(host_public) = 32)),
    CONSTRAINT environment_vault_hosts_installation_generation_check CHECK (((installation_generation >= 1) AND (installation_generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_vault_hosts_observation_check CHECK (((observation IS NULL) OR (octet_length(observation) <= 4096))),
    CONSTRAINT environment_vault_hosts_observation_seq_check CHECK (((observation_seq >= 0) AND (observation_seq <= '9007199254740991'::bigint))),
    CONSTRAINT environment_vault_hosts_projection_revision_check CHECK (((projection_revision >= 0) AND (projection_revision <= '9007199254740991'::bigint))),
    CONSTRAINT environment_vault_hosts_selection_check CHECK (((octet_length(selection) >= 2) AND (octet_length(selection) <= 65536))),
    CONSTRAINT environment_vault_hosts_selection_generation_check CHECK (((selection_generation >= 0) AND (selection_generation <= '9007199254740991'::bigint))),
    CONSTRAINT environment_vault_hosts_writer_public_check CHECK ((octet_length(writer_public) = 32))
);


--
-- Name: environment_vault_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_vault_operations (
    account_id text NOT NULL,
    operation_id text NOT NULL,
    request_digest bytea NOT NULL,
    result bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT environment_vault_operations_request_digest_check CHECK ((octet_length(request_digest) = 32)),
    CONSTRAINT environment_vault_operations_result_check CHECK (((octet_length(result) >= 1) AND (octet_length(result) <= 2097152)))
);


--
-- Name: environment_vault_personal_epochs; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_vault_personal_epochs (
    account_id text NOT NULL,
    key_epoch bigint NOT NULL,
    rotation_required boolean DEFAULT false NOT NULL,
    CONSTRAINT environment_vault_personal_epochs_key_epoch_check CHECK (((key_epoch >= 1) AND (key_epoch <= '9007199254740991'::bigint)))
);


--
-- Name: environment_vault_personal_rotation_scopes; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_vault_personal_rotation_scopes (
    account_id text NOT NULL,
    operation_id text NOT NULL,
    machine_id text DEFAULT ''::text NOT NULL,
    document_id text NOT NULL,
    envelope bytea NOT NULL,
    CONSTRAINT environment_vault_personal_rotation_scopes_document_id_check CHECK ((document_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_vault_personal_rotation_scopes_envelope_check CHECK (((octet_length(envelope) >= 1) AND (octet_length(envelope) <= 264192)))
);


--
-- Name: environment_vault_personal_rotations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_vault_personal_rotations (
    account_id text NOT NULL,
    operation_id text NOT NULL,
    expected_vault_document_id text NOT NULL
);


--
-- Name: environment_vault_projection_fences; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_vault_projection_fences (
    machine_id text NOT NULL,
    generation bigint NOT NULL,
    CONSTRAINT environment_vault_projection_fences_generation_check CHECK (((generation >= 1) AND (generation <= '9007199254740991'::bigint)))
);


--
-- Name: environment_vault_projection_sources; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_vault_projection_sources (
    machine_id text NOT NULL,
    owner_kind text NOT NULL,
    owner_id text NOT NULL,
    source_machine_id text DEFAULT ''::text NOT NULL,
    CONSTRAINT environment_vault_projection_sources_owner_kind_check CHECK ((owner_kind = ANY (ARRAY['personal'::text, 'team'::text])))
);


--
-- Name: environment_vault_scopes; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_vault_scopes (
    owner_kind text NOT NULL,
    owner_id text NOT NULL,
    machine_id text DEFAULT ''::text NOT NULL,
    key_epoch bigint NOT NULL,
    revision bigint NOT NULL,
    document_id text NOT NULL,
    envelope bytea NOT NULL,
    writer_public bytea NOT NULL,
    CONSTRAINT environment_vault_scopes_check CHECK (((owner_kind = 'personal'::text) OR (machine_id = ''::text))),
    CONSTRAINT environment_vault_scopes_document_id_check CHECK ((document_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_vault_scopes_envelope_check CHECK (((octet_length(envelope) >= 1) AND (octet_length(envelope) <= 264192))),
    CONSTRAINT environment_vault_scopes_key_epoch_check CHECK (((key_epoch >= 1) AND (key_epoch <= '9007199254740991'::bigint))),
    CONSTRAINT environment_vault_scopes_owner_kind_check CHECK ((owner_kind = ANY (ARRAY['personal'::text, 'team'::text]))),
    CONSTRAINT environment_vault_scopes_revision_check CHECK (((revision >= 1) AND (revision <= '9007199254740991'::bigint))),
    CONSTRAINT environment_vault_scopes_writer_public_check CHECK ((octet_length(writer_public) = 32))
);


--
-- Name: environment_vault_team_grants; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_vault_team_grants (
    team_id text NOT NULL,
    account_id text NOT NULL,
    membership_generation bigint NOT NULL,
    team_epoch bigint NOT NULL,
    recipient_vault_generation bigint NOT NULL,
    recipient_sharing_public bytea NOT NULL,
    document_id text NOT NULL,
    envelope bytea NOT NULL,
    sender_writer_public bytea NOT NULL,
    acknowledged boolean DEFAULT false NOT NULL,
    CONSTRAINT environment_vault_team_grants_document_id_check CHECK ((document_id ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_vault_team_grants_envelope_check CHECK (((octet_length(envelope) >= 1) AND (octet_length(envelope) <= 4096))),
    CONSTRAINT environment_vault_team_grants_recipient_sharing_public_check CHECK ((octet_length(recipient_sharing_public) = 32)),
    CONSTRAINT environment_vault_team_grants_sender_writer_public_check CHECK ((octet_length(sender_writer_public) = 32))
);


--
-- Name: environment_vault_team_members; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_vault_team_members (
    team_id text NOT NULL,
    account_id text NOT NULL,
    grant_epoch bigint NOT NULL,
    CONSTRAINT environment_vault_team_members_grant_epoch_check CHECK (((grant_epoch >= 0) AND (grant_epoch <= '9007199254740991'::bigint)))
);


--
-- Name: environment_vault_teams; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.environment_vault_teams (
    team_id text NOT NULL,
    key_epoch bigint NOT NULL,
    rotation_required boolean DEFAULT false NOT NULL,
    CONSTRAINT environment_vault_teams_key_epoch_check CHECK (((key_epoch >= 1) AND (key_epoch <= '9007199254740991'::bigint)))
);


--
-- Name: github_config_repositories; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.github_config_repositories (
    id text NOT NULL,
    user_id text NOT NULL,
    provider_repo_id text DEFAULT ''::text NOT NULL,
    owner text NOT NULL,
    name text NOT NULL,
    default_branch text NOT NULL,
    clone_url text NOT NULL,
    html_url text DEFAULT ''::text NOT NULL,
    private boolean DEFAULT true NOT NULL,
    provisioned_at timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: github_oauth_tokens; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.github_oauth_tokens (
    id text NOT NULL,
    user_id text NOT NULL,
    token_ciphertext bytea NOT NULL,
    scopes text[] DEFAULT '{}'::text[] NOT NULL,
    expires_at timestamp with time zone,
    refresh_expires_at timestamp with time zone,
    revoked_at timestamp with time zone,
    last_validated_at timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    refresh_token_ciphertext bytea,
    provider_account_login text DEFAULT ''::text NOT NULL
);


--
-- Name: github_repo_provisioning_attempts; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.github_repo_provisioning_attempts (
    id text NOT NULL,
    user_id text NOT NULL,
    idempotency_key text NOT NULL,
    state text NOT NULL,
    repo_owner text DEFAULT ''::text NOT NULL,
    repo_name text DEFAULT ''::text NOT NULL,
    last_error text DEFAULT ''::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_retry_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: helper_identity_renewals; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.helper_identity_renewals (
    operation_key text NOT NULL,
    helper_id text NOT NULL,
    environment_id text NOT NULL,
    request_hash bytea NOT NULL,
    identity_ciphertext bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT hosted_helper_identity_renewals_operation_key_check CHECK (((length(operation_key) >= 8) AND (length(operation_key) <= 320)))
);


--
-- Name: helper_revocation_outbox; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.helper_revocation_outbox (
    id text NOT NULL,
    user_id text NOT NULL,
    project_id text NOT NULL,
    cli_client_session_id text NOT NULL,
    http_base_url text NOT NULL,
    session_ids text[] NOT NULL,
    reason text NOT NULL,
    propagated_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT helper_revocation_outbox_session_ids_check CHECK ((cardinality(session_ids) > 0))
);


--
-- Name: inspector_credentials; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.inspector_credentials (
    credential_id text NOT NULL,
    token_hash bytea NOT NULL,
    account_id text NOT NULL,
    resource_kind text NOT NULL,
    resource_id text NOT NULL,
    route_id text NOT NULL,
    action text NOT NULL,
    resource_generation bigint NOT NULL,
    route_generation bigint NOT NULL,
    target_generation bigint NOT NULL,
    owner_account_id text NOT NULL,
    team_id text,
    team_generation bigint,
    membership_generation bigint,
    binding_generation bigint,
    grant_generation bigint,
    issued_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    native_cli_session_id text,
    native_machine_id text,
    native_machine_generation bigint,
    CONSTRAINT inspector_credentials_action_check CHECK ((action = ANY (ARRAY['inspect'::text, 'replay'::text]))),
    CONSTRAINT inspector_credentials_check CHECK (((expires_at > issued_at) AND (expires_at <= (issued_at + '00:05:00'::interval)))),
    CONSTRAINT inspector_credentials_check1 CHECK (((team_id IS NULL) OR ((team_id IS NOT NULL) AND (team_generation > 0) AND (membership_generation > 0) AND (binding_generation > 0) AND (grant_generation > 0)))),
    CONSTRAINT inspector_credentials_native_machine_generation_check CHECK ((native_machine_generation > 0)),
    CONSTRAINT inspector_credentials_resource_generation_check CHECK ((resource_generation > 0)),
    CONSTRAINT inspector_credentials_resource_kind_check CHECK ((resource_kind = ANY (ARRAY['preview'::text, 'tunnel'::text]))),
    CONSTRAINT inspector_credentials_route_generation_check CHECK ((route_generation > 0)),
    CONSTRAINT inspector_credentials_target_generation_check CHECK ((target_generation > 0)),
    CONSTRAINT inspector_credentials_token_hash_check CHECK ((octet_length(token_hash) = 32))
);


--
-- Name: preview_lease_carrier_attachments; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.preview_lease_carrier_attachments (
    account_id text NOT NULL,
    preview_id text NOT NULL,
    operation_id text NOT NULL,
    idempotency_key text NOT NULL,
    request_id text NOT NULL,
    correlation_id text NOT NULL,
    request_hash bytea NOT NULL,
    owner_device_id text NOT NULL,
    owner_session_id text NOT NULL,
    host_id text NOT NULL,
    edge_node_id text NOT NULL,
    machine_identity_public_key text NOT NULL,
    machine_identity_thumbprint text NOT NULL,
    carrier_kind text DEFAULT 'preview_ephemeral'::text NOT NULL,
    lease_generation bigint NOT NULL,
    tunnel_id text NOT NULL,
    connector_id text NOT NULL,
    connector_session_id text NOT NULL,
    process_generation bigint NOT NULL,
    config_generation bigint NOT NULL,
    route_id text NOT NULL,
    route_generation bigint NOT NULL,
    config_content_hash text NOT NULL,
    edge_endpoints text[] NOT NULL,
    attachment_generation bigint DEFAULT 1 NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    edge_ready boolean DEFAULT false NOT NULL,
    origin_ready boolean DEFAULT false NOT NULL,
    issued_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    ready_at timestamp with time zone,
    released_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    edge_process_epoch text NOT NULL,
    edge_carrier_server_spki_sha256 text,
    edge_carrier_server_certificate_chain_pem text,
    CONSTRAINT preview_lease_carrier_attachm_machine_identity_public_key_check CHECK ((machine_identity_public_key ~ '^[A-Za-z0-9_-]{43}$'::text)),
    CONSTRAINT preview_lease_carrier_attachm_machine_identity_thumbprint_check CHECK ((machine_identity_thumbprint ~ '^sha256:[A-Za-z0-9_-]{43}$'::text)),
    CONSTRAINT preview_lease_carrier_attachments_attachment_generation_check CHECK ((attachment_generation > 0)),
    CONSTRAINT preview_lease_carrier_attachments_carrier_kind_check CHECK ((carrier_kind = 'preview_ephemeral'::text)),
    CONSTRAINT preview_lease_carrier_attachments_check CHECK ((host_id = owner_device_id)),
    CONSTRAINT preview_lease_carrier_attachments_check1 CHECK ((expires_at > issued_at)),
    CONSTRAINT preview_lease_carrier_attachments_check2 CHECK (((state = 'ready'::text) = (edge_ready AND origin_ready))),
    CONSTRAINT preview_lease_carrier_attachments_check3 CHECK (((NOT origin_ready) OR edge_ready)),
    CONSTRAINT preview_lease_carrier_attachments_check4 CHECK (((state <> ALL (ARRAY['pending'::text, 'admitted'::text])) OR ((NOT edge_ready) AND (NOT origin_ready)))),
    CONSTRAINT preview_lease_carrier_attachments_check5 CHECK (((state <> 'edge_ready'::text) OR (edge_ready AND (NOT origin_ready)))),
    CONSTRAINT preview_lease_carrier_attachments_check6 CHECK (((state = 'released'::text) = (released_at IS NOT NULL))),
    CONSTRAINT preview_lease_carrier_attachments_check7 CHECK (((state = 'ready'::text) = (ready_at IS NOT NULL))),
    CONSTRAINT preview_lease_carrier_attachments_check8 CHECK (((state <> 'released'::text) OR ((NOT edge_ready) AND (NOT origin_ready)))),
    CONSTRAINT preview_lease_carrier_attachments_config_content_hash_check CHECK ((config_content_hash ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT preview_lease_carrier_attachments_config_generation_check CHECK ((config_generation > 0)),
    CONSTRAINT preview_lease_carrier_attachments_correlation_id_check CHECK (((length(TRIM(BOTH FROM correlation_id)) >= 3) AND (length(TRIM(BOTH FROM correlation_id)) <= 128))),
    CONSTRAINT preview_lease_carrier_attachments_edge_carrier_chain_check CHECK (((octet_length(edge_carrier_server_certificate_chain_pem) >= 1) AND (octet_length(edge_carrier_server_certificate_chain_pem) <= 65536))),
    CONSTRAINT preview_lease_carrier_attachments_edge_carrier_spki_check CHECK ((edge_carrier_server_spki_sha256 ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT preview_lease_carrier_attachments_edge_endpoints_check CHECK ((cardinality(edge_endpoints) = 2)),
    CONSTRAINT preview_lease_carrier_attachments_edge_node_id_check CHECK ((edge_node_id <> ''::text)),
    CONSTRAINT preview_lease_carrier_attachments_edge_process_epoch_check CHECK ((((length(edge_process_epoch) >= 8) AND (length(edge_process_epoch) <= 128)) AND (edge_process_epoch ~ '^[A-Za-z0-9_-]+$'::text))),
    CONSTRAINT preview_lease_carrier_attachments_idempotency_key_check CHECK (((length(TRIM(BOTH FROM idempotency_key)) >= 1) AND (length(TRIM(BOTH FROM idempotency_key)) <= 256))),
    CONSTRAINT preview_lease_carrier_attachments_lease_generation_check CHECK ((lease_generation > 0)),
    CONSTRAINT preview_lease_carrier_attachments_process_generation_check CHECK ((process_generation > 0)),
    CONSTRAINT preview_lease_carrier_attachments_request_hash_check CHECK ((octet_length(request_hash) = 32)),
    CONSTRAINT preview_lease_carrier_attachments_request_id_check CHECK (((length(TRIM(BOTH FROM request_id)) >= 3) AND (length(TRIM(BOTH FROM request_id)) <= 128))),
    CONSTRAINT preview_lease_carrier_attachments_route_generation_check CHECK ((route_generation > 0)),
    CONSTRAINT preview_lease_carrier_attachments_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'admitted'::text, 'edge_ready'::text, 'ready'::text, 'failed'::text, 'released'::text])))
);


--
-- Name: TABLE preview_lease_carrier_attachments; Type: COMMENT; Schema: paperboat; Owner: -
--

COMMENT ON TABLE paperboat.preview_lease_carrier_attachments IS 'Ephemeral preview-to-canonical-carrier binding metadata. Never stores bearer or private material.';


--
-- Name: COLUMN preview_lease_carrier_attachments.request_hash; Type: COMMENT; Schema: paperboat; Owner: -
--

COMMENT ON COLUMN paperboat.preview_lease_carrier_attachments.request_hash IS 'SHA-256 of the canonical attachment request envelope; not secret material.';


--
-- Name: preview_leases; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.preview_leases (
    id text NOT NULL,
    endpoint_id text NOT NULL,
    endpoint text NOT NULL,
    account_id text NOT NULL,
    actor_id text NOT NULL,
    owner_device_id text NOT NULL,
    owner_session_id text NOT NULL,
    target_scheme text NOT NULL,
    target_address text NOT NULL,
    access_mode text DEFAULT 'public'::text NOT NULL,
    lease_deadline timestamp with time zone NOT NULL,
    user_deadline timestamp with time zone,
    allocation_state text DEFAULT 'pending'::text NOT NULL,
    edge_state text DEFAULT 'pending'::text NOT NULL,
    origin_state text DEFAULT 'unknown'::text NOT NULL,
    terminal_state text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    ready_at timestamp with time zone,
    last_renewed_at timestamp with time zone DEFAULT now() NOT NULL,
    stopped_at timestamp with time zone,
    generation bigint DEFAULT 1 NOT NULL,
    owner_last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT preview_leases_access_mode_check CHECK ((access_mode = ANY (ARRAY['public'::text, 'private'::text, 'team'::text]))),
    CONSTRAINT preview_leases_allocation_state_check CHECK ((allocation_state = ANY (ARRAY['pending'::text, 'ready'::text, 'failed'::text, 'released'::text]))),
    CONSTRAINT preview_leases_check CHECK ((lease_deadline > created_at)),
    CONSTRAINT preview_leases_check1 CHECK (((user_deadline IS NULL) OR (user_deadline >= created_at))),
    CONSTRAINT preview_leases_check2 CHECK (((terminal_state = 'active'::text) = (stopped_at IS NULL))),
    CONSTRAINT preview_leases_edge_state_check CHECK ((edge_state = ANY (ARRAY['pending'::text, 'ready'::text, 'degraded'::text, 'down'::text, 'released'::text]))),
    CONSTRAINT preview_leases_endpoint_check CHECK ((endpoint ~ '^https://[a-z0-9.-]+$'::text)),
    CONSTRAINT preview_leases_generation_positive CHECK ((generation > 0)),
    CONSTRAINT preview_leases_origin_state_check CHECK ((origin_state = ANY (ARRAY['unknown'::text, 'ready'::text, 'degraded'::text, 'down'::text]))),
    CONSTRAINT preview_leases_target_address_check CHECK (((length(TRIM(BOTH FROM target_address)) >= 1) AND (length(TRIM(BOTH FROM target_address)) <= 512))),
    CONSTRAINT preview_leases_target_scheme_v1_check CHECK ((target_scheme = ANY (ARRAY['http'::text, 'https'::text, 'h2c'::text, 'unix'::text, 'tcp'::text]))),
    CONSTRAINT preview_leases_terminal_state_check CHECK ((terminal_state = ANY (ARRAY['active'::text, 'stopped'::text, 'expired'::text, 'owner_lost'::text, 'failed'::text])))
);


--
-- Name: team_members; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_members (
    team_id text NOT NULL,
    account_id text NOT NULL,
    membership_generation bigint NOT NULL,
    role text NOT NULL,
    active boolean NOT NULL,
    CONSTRAINT team_members_membership_generation_check CHECK (((membership_generation >= 1) AND (membership_generation <= '9007199254740991'::bigint))),
    CONSTRAINT team_members_role_check CHECK ((role = ANY (ARRAY['admin'::text, 'member'::text])))
);


--
-- Name: team_resource_bindings; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_resource_bindings (
    team_id text NOT NULL,
    resource_kind text NOT NULL,
    resource_id text NOT NULL,
    owner_account text NOT NULL,
    active boolean DEFAULT true NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    terminal_process_generation bigint,
    CONSTRAINT team_resource_bindings_generation_check CHECK (((generation >= 1) AND (generation <= '9007199254740991'::bigint))),
    CONSTRAINT team_resource_bindings_resource_kind_check CHECK ((resource_kind = ANY (ARRAY['env'::text, 'preview'::text, 'tunnel'::text, 'lazy_policy'::text, 'machine'::text, 'terminal_session'::text])))
);


--
-- Name: team_resource_grants; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_resource_grants (
    team_id text NOT NULL,
    account_id text NOT NULL,
    resource_kind text NOT NULL,
    resource_id text NOT NULL,
    permission text NOT NULL,
    generation bigint NOT NULL,
    active boolean NOT NULL,
    CONSTRAINT team_resource_grants_check CHECK ((((resource_kind = 'env'::text) AND (permission = ANY (ARRAY['read'::text, 'write'::text]))) OR ((resource_kind = ANY (ARRAY['preview'::text, 'tunnel'::text])) AND (permission = ANY (ARRAY['use'::text, 'manage'::text, 'inspect'::text, 'replay'::text]))) OR ((resource_kind = 'lazy_policy'::text) AND (permission = ANY (ARRAY['use'::text, 'manage'::text]))))),
    CONSTRAINT team_resource_grants_generation_check CHECK (((generation >= 1) AND (generation <= '9007199254740991'::bigint)))
);


--
-- Name: teams; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.teams (
    team_id text NOT NULL,
    owner_account text NOT NULL,
    generation bigint NOT NULL,
    deleted_at timestamp with time zone,
    suspended boolean DEFAULT false NOT NULL,
    suspended_at timestamp with time zone,
    suspended_by text,
    CONSTRAINT teams_generation_check CHECK (((generation >= 1) AND (generation <= '9007199254740991'::bigint))),
    CONSTRAINT teams_suspension_state CHECK (((suspended AND (suspended_at IS NOT NULL) AND (suspended_by IS NOT NULL)) OR ((NOT suspended) AND (suspended_at IS NULL) AND (suspended_by IS NULL))))
);


--
-- Name: tunnel_routes; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_routes (
    id text NOT NULL,
    tunnel_id text NOT NULL,
    name text NOT NULL,
    protocol text DEFAULT 'http'::text NOT NULL,
    match_type text NOT NULL,
    match_hostname text,
    wildcard_suffix text,
    path_prefix text,
    priority integer DEFAULT 100 NOT NULL,
    origin_scheme text NOT NULL,
    origin_address text NOT NULL,
    preserve_host boolean DEFAULT true NOT NULL,
    host_override text,
    tls_verification text DEFAULT 'not_applicable'::text NOT NULL,
    tls_server_name text,
    ca_reference text,
    mtls_credential_reference text,
    connect_timeout_ms integer DEFAULT 10000 NOT NULL,
    idle_timeout_ms integer DEFAULT 90000 NOT NULL,
    max_concurrent_streams integer DEFAULT 128 NOT NULL,
    desired_state text DEFAULT 'active'::text NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    created_by_actor_id text NOT NULL,
    updated_by_actor_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    public_tcp_listener_id text,
    public_tcp_port integer,
    CONSTRAINT tunnel_routes_check CHECK ((((match_type = ANY (ARRAY['managed'::text, 'exact'::text])) AND (match_hostname IS NOT NULL) AND (wildcard_suffix IS NULL)) OR ((match_type = 'one_label_wildcard'::text) AND (match_hostname IS NULL) AND (wildcard_suffix IS NOT NULL)) OR ((match_type = 'catch_all'::text) AND (match_hostname IS NULL) AND (wildcard_suffix IS NULL)))),
    CONSTRAINT tunnel_routes_check1 CHECK (((origin_scheme = 'https'::text) OR (tls_verification = 'not_applicable'::text))),
    CONSTRAINT tunnel_routes_check2 CHECK (((desired_state = 'deleted'::text) = (deleted_at IS NOT NULL))),
    CONSTRAINT tunnel_routes_connect_timeout_ms_check CHECK (((connect_timeout_ms >= 100) AND (connect_timeout_ms <= 120000))),
    CONSTRAINT tunnel_routes_desired_state_check CHECK ((desired_state = ANY (ARRAY['active'::text, 'disabled'::text, 'deleted'::text]))),
    CONSTRAINT tunnel_routes_generation_check CHECK ((generation > 0)),
    CONSTRAINT tunnel_routes_idle_timeout_ms_check CHECK (((idle_timeout_ms >= 1000) AND (idle_timeout_ms <= 3600000))),
    CONSTRAINT tunnel_routes_match_hostname_check CHECK (((match_hostname IS NULL) OR ((match_hostname = lower(match_hostname)) AND (match_hostname !~ '[.]$'::text) AND (match_hostname !~ '[*]'::text)))),
    CONSTRAINT tunnel_routes_match_type_check CHECK ((match_type = ANY (ARRAY['managed'::text, 'exact'::text, 'one_label_wildcard'::text, 'catch_all'::text]))),
    CONSTRAINT tunnel_routes_max_concurrent_streams_check CHECK (((max_concurrent_streams >= 1) AND (max_concurrent_streams <= 100000))),
    CONSTRAINT tunnel_routes_name_check CHECK (((length(TRIM(BOTH FROM name)) >= 1) AND (length(TRIM(BOTH FROM name)) <= 80))),
    CONSTRAINT tunnel_routes_origin_address_check CHECK (((length(TRIM(BOTH FROM origin_address)) >= 1) AND (length(TRIM(BOTH FROM origin_address)) <= 512))),
    CONSTRAINT tunnel_routes_origin_scheme_check CHECK ((origin_scheme = ANY (ARRAY['http'::text, 'https'::text, 'h2c'::text, 'unix'::text, 'tcp'::text]))),
    CONSTRAINT tunnel_routes_path_prefix_check CHECK (((path_prefix IS NULL) OR (path_prefix ~~ '/%'::text))),
    CONSTRAINT tunnel_routes_protocol_check CHECK ((protocol = ANY (ARRAY['http'::text, 'tcp'::text, 'tls'::text, 'private_tcp'::text]))),
    CONSTRAINT tunnel_routes_public_tcp_binding_check CHECK ((((protocol = ANY (ARRAY['tcp'::text, 'tls'::text])) AND (public_tcp_listener_id IS NOT NULL) AND ((length(TRIM(BOTH FROM public_tcp_listener_id)) >= 3) AND (length(TRIM(BOTH FROM public_tcp_listener_id)) <= 128)) AND (public_tcp_port IS NOT NULL) AND (((protocol = 'tcp'::text) AND ((public_tcp_port >= 1024) AND (public_tcp_port <= 65535))) OR ((protocol = 'tls'::text) AND (public_tcp_port = 443) AND (public_tcp_listener_id = 'listener_tls_443'::text))) AND (match_type = 'managed'::text) AND (match_hostname IS NOT NULL) AND (wildcard_suffix IS NULL) AND (origin_scheme = 'tcp'::text) AND (path_prefix IS NULL) AND (preserve_host = false) AND (host_override IS NULL)) OR ((protocol <> ALL (ARRAY['tcp'::text, 'tls'::text])) AND (public_tcp_listener_id IS NULL) AND (public_tcp_port IS NULL)))),
    CONSTRAINT tunnel_routes_tls_verification_check CHECK ((tls_verification = ANY (ARRAY['not_applicable'::text, 'system'::text, 'custom_ca'::text, 'mutual_tls'::text, 'insecure_development'::text]))),
    CONSTRAINT tunnel_routes_wildcard_suffix_check CHECK (((wildcard_suffix IS NULL) OR ((wildcard_suffix = lower(wildcard_suffix)) AND (wildcard_suffix !~ '[.]$'::text) AND (wildcard_suffix !~ '[*]'::text))))
);


--
-- Name: tunnels; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnels (
    id text NOT NULL,
    account_id text NOT NULL,
    name text NOT NULL,
    desired_state text DEFAULT 'active'::text NOT NULL,
    access_mode text DEFAULT 'public'::text NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    stable_endpoint_id text NOT NULL,
    stable_endpoint text NOT NULL,
    created_by_host_id text NOT NULL,
    created_by_actor_id text NOT NULL,
    expires_at timestamp with time zone,
    summary_code text DEFAULT 'pending'::text NOT NULL,
    summary_transitioned_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    CONSTRAINT tunnels_access_mode_check CHECK ((access_mode = ANY (ARRAY['public'::text, 'private'::text, 'team'::text]))),
    CONSTRAINT tunnels_check CHECK ((stable_endpoint ~ (('^https://'::text || stable_endpoint_id) || '[.][a-z0-9.-]+$'::text))),
    CONSTRAINT tunnels_check1 CHECK (((desired_state = 'deleted'::text) = (deleted_at IS NOT NULL))),
    CONSTRAINT tunnels_check2 CHECK (((expires_at IS NULL) OR (expires_at > created_at))),
    CONSTRAINT tunnels_desired_state_check CHECK ((desired_state = ANY (ARRAY['active'::text, 'paused'::text, 'deleted'::text]))),
    CONSTRAINT tunnels_generation_check CHECK ((generation > 0)),
    CONSTRAINT tunnels_name_check CHECK (((length(TRIM(BOTH FROM name)) >= 1) AND (length(TRIM(BOTH FROM name)) <= 80))),
    CONSTRAINT tunnels_stable_endpoint_id_check CHECK ((stable_endpoint_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'::text))
);


--
-- Name: user_machines; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_machines (
    id text NOT NULL,
    user_id text NOT NULL,
    environment_id text NOT NULL,
    platform text NOT NULL,
    architecture text NOT NULL,
    workspace_root text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    seat_state text DEFAULT 'reserved'::text NOT NULL,
    online boolean DEFAULT false NOT NULL,
    provider_route_route_id text,
    provider_route_client_id text,
    provider_route_http_base_url text,
    provider_route_websocket_base_url text,
    runtime_versions jsonb DEFAULT '{}'::jsonb NOT NULL,
    enrolled_at timestamp with time zone,
    last_seen_at timestamp with time zone,
    revoked_at timestamp with time zone,
    disconnected_at timestamp with time zone,
    deleted_at timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    availability_mode text DEFAULT 'allow_sleep'::text NOT NULL,
    availability_desired_version bigint DEFAULT 0 NOT NULL,
    availability_observed_mode text,
    availability_observed_version bigint DEFAULT 0 NOT NULL,
    availability_observed_at timestamp with time zone,
    availability_status text DEFAULT 'pending'::text NOT NULL,
    availability_error_code text,
    host_service_version text,
    host_service_scope text,
    worker_generation bigint DEFAULT 0 NOT NULL,
    os_boot_id text,
    worker_service_scope text DEFAULT 'unknown'::text NOT NULL,
    connector_state text DEFAULT 'unavailable'::text NOT NULL,
    connector_generation bigint DEFAULT 0 NOT NULL,
    host_update_rollbacks bigint DEFAULT 0 NOT NULL,
    runtime_diagnostics_observed_at timestamp with time zone,
    setup_roles text[] DEFAULT ARRAY['host'::text] NOT NULL,
    public_identity_key text,
    installation_generation bigint DEFAULT 1 NOT NULL,
    setup_mode text DEFAULT 'host'::text NOT NULL,
    configured_capabilities text[] DEFAULT ARRAY['file_receive'::text, 'preview_launch'::text, 'terminal_host'::text, 'codex_host'::text, 'session_host'::text, 'ssh_host'::text] NOT NULL,
    observed_capabilities text[] DEFAULT '{}'::text[] NOT NULL,
    alias text NOT NULL,
    update_health text DEFAULT 'unknown'::text NOT NULL,
    capabilities_desired_version bigint DEFAULT 1 NOT NULL,
    capabilities_observed_version bigint DEFAULT 0 NOT NULL,
    capabilities_status text DEFAULT 'pending'::text NOT NULL,
    capabilities_error_code text,
    owner_team_id text,
    description text DEFAULT ''::text NOT NULL,
    personal_auto_enabled boolean DEFAULT true NOT NULL,
    team_auto_enabled boolean DEFAULT false NOT NULL,
    device_service_policy_generation bigint DEFAULT 1 NOT NULL,
    CONSTRAINT user_machines_alias_format CHECK ((alias ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'::text)),
    CONSTRAINT user_machines_availability_desired_version_check CHECK ((availability_desired_version >= 0)),
    CONSTRAINT user_machines_availability_mode_check CHECK ((availability_mode = ANY (ARRAY['allow_sleep'::text, 'keep_awake'::text]))),
    CONSTRAINT user_machines_availability_observed_mode_check CHECK (((availability_observed_mode IS NULL) OR (availability_observed_mode = ANY (ARRAY['allow_sleep'::text, 'keep_awake'::text])))),
    CONSTRAINT user_machines_availability_observed_version_check CHECK ((availability_observed_version >= 0)),
    CONSTRAINT user_machines_availability_status_check CHECK ((availability_status = ANY (ARRAY['applied'::text, 'pending'::text, 'unsupported'::text, 'error'::text]))),
    CONSTRAINT user_machines_capabilities_desired_version_check CHECK ((capabilities_desired_version > 0)),
    CONSTRAINT user_machines_capabilities_observed_version_check CHECK ((capabilities_observed_version >= 0)),
    CONSTRAINT user_machines_capabilities_status_check CHECK ((capabilities_status = ANY (ARRAY['pending'::text, 'applied'::text, 'offline'::text, 'error'::text]))),
    CONSTRAINT user_machines_configured_capabilities_check CHECK ((configured_capabilities <@ ARRAY['file_receive'::text, 'preview_launch'::text, 'terminal_host'::text, 'codex_host'::text, 'session_host'::text, 'ssh_host'::text, 'keep_awake'::text, 'environment_injection'::text, 'peer_relay'::text])),
    CONSTRAINT user_machines_connector_generation_check CHECK ((connector_generation >= 0)),
    CONSTRAINT user_machines_connector_state_check CHECK ((connector_state = ANY (ARRAY['ready'::text, 'degraded'::text, 'unavailable'::text]))),
    CONSTRAINT user_machines_description_bound CHECK (((char_length(description) <= 160) AND (description !~ '[[:cntrl:]]'::text))),
    CONSTRAINT user_machines_device_service_policy_generation_check CHECK (((device_service_policy_generation >= 1) AND (device_service_policy_generation <= '9007199254740991'::bigint))),
    CONSTRAINT user_machines_host_service_scope_check CHECK (((host_service_scope IS NULL) OR (host_service_scope = 'system'::text))),
    CONSTRAINT user_machines_host_update_rollbacks_check CHECK ((host_update_rollbacks >= 0)),
    CONSTRAINT user_machines_installation_generation_check CHECK ((installation_generation > 0)),
    CONSTRAINT user_machines_observed_capabilities_check CHECK ((observed_capabilities <@ ARRAY['file_receive'::text, 'preview_launch'::text, 'terminal_host'::text, 'codex_host'::text, 'session_host'::text, 'ssh_host'::text, 'keep_awake'::text, 'environment_injection'::text, 'peer_relay'::text])),
    CONSTRAINT user_machines_public_identity_key_check CHECK (((public_identity_key IS NULL) OR ((length(public_identity_key) >= 40) AND (length(public_identity_key) <= 256)))),
    CONSTRAINT user_machines_seat_state_check CHECK ((seat_state = ANY (ARRAY['reserved'::text, 'occupied'::text, 'released'::text]))),
    CONSTRAINT user_machines_setup_mode_check CHECK ((setup_mode = ANY (ARRAY['client'::text, 'host'::text]))),
    CONSTRAINT user_machines_setup_roles_check CHECK ((setup_roles <@ ARRAY['interactive'::text, 'host'::text])),
    CONSTRAINT user_machines_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'online'::text, 'offline'::text, 'disconnected'::text, 'revoked'::text, 'deleted'::text]))),
    CONSTRAINT user_machines_update_health_check CHECK ((update_health = ANY (ARRAY['unknown'::text, 'healthy'::text, 'recovery_required'::text]))),
    CONSTRAINT user_machines_worker_generation_check CHECK ((worker_generation >= 0)),
    CONSTRAINT user_machines_worker_service_scope_check CHECK ((worker_service_scope = ANY (ARRAY['unknown'::text, 'user'::text, 'system'::text]))),
    CONSTRAINT user_machines_workspace_root_check CHECK (((workspace_root ~ '^/'::text) OR ((workspace_root ~ '^[A-Za-z]:'::text) AND (SUBSTRING(workspace_root FROM 3 FOR 1) = chr(92))) OR (SUBSTRING(workspace_root FROM 1 FOR 2) = (chr(92) || chr(92)))))
);


--
-- Name: users; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.users (
    id text NOT NULL,
    workos_subject text NOT NULL,
    primary_email text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    status text NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    role text DEFAULT 'user'::text NOT NULL,
    CONSTRAINT users_role_valid CHECK ((role = ANY (ARRAY['user'::text, 'support'::text, 'admin'::text, 'system_worker'::text, 'billing'::text, 'operations'::text])))
);


--
-- Name: inspector_native_admissions; Type: VIEW; Schema: paperboat; Owner: -
--

CREATE VIEW paperboat.inspector_native_admissions AS
 SELECT i.credential_id,
    i.account_id,
    i.owner_account_id,
    i.native_cli_session_id,
    i.native_machine_id,
    i.expires_at
   FROM ((((paperboat.inspector_credentials i
     JOIN paperboat.cli_client_sessions c ON (((c.id = i.native_cli_session_id) AND (c.user_id = i.account_id) AND (c.state = 'active'::text) AND (c.revoked_at IS NULL))))
     JOIN paperboat.user_machines m ON (((m.id = i.native_machine_id) AND (m.user_id = i.owner_account_id) AND (m.installation_generation = i.native_machine_generation) AND (m.deleted_at IS NULL) AND (m.revoked_at IS NULL))))
     JOIN paperboat.users actor ON (((actor.id = i.account_id) AND (actor.status = 'active'::text))))
     JOIN paperboat.users owner ON (((owner.id = i.owner_account_id) AND (owner.status = 'active'::text))))
  WHERE ((i.revoked_at IS NULL) AND (i.expires_at > now()) AND (((i.team_id IS NULL) AND (i.account_id = i.owner_account_id)) OR (EXISTS ( SELECT 1
           FROM (((paperboat.teams t
             JOIN paperboat.team_members tm ON (((tm.team_id = t.team_id) AND (tm.account_id = i.account_id))))
             JOIN paperboat.team_resource_bindings b ON (((b.team_id = t.team_id) AND (b.resource_kind = i.resource_kind) AND (b.resource_id = i.resource_id))))
             JOIN paperboat.team_resource_grants g ON (((g.team_id = t.team_id) AND (g.account_id = i.account_id) AND (g.resource_kind = i.resource_kind) AND (g.resource_id = i.resource_id) AND (g.permission = i.action))))
          WHERE ((t.team_id = i.team_id) AND (t.deleted_at IS NULL) AND (t.generation = i.team_generation) AND tm.active AND (tm.membership_generation = i.membership_generation) AND b.active AND (b.owner_account = i.owner_account_id) AND (b.generation = i.binding_generation) AND g.active AND (g.generation = i.grant_generation))))) AND (((i.resource_kind = 'preview'::text) AND (EXISTS ( SELECT 1
           FROM (paperboat.preview_leases p
             JOIN paperboat.preview_lease_carrier_attachments x ON (((x.preview_id = p.id) AND (x.account_id = p.account_id))))
          WHERE ((p.id = i.resource_id) AND (p.account_id = i.owner_account_id) AND (p.terminal_state = 'active'::text) AND (p.lease_deadline > now()) AND ((p.user_deadline IS NULL) OR (p.user_deadline > now())) AND (x.state = 'ready'::text) AND (x.expires_at > now()) AND (x.host_id = i.native_machine_id) AND (x.route_generation = i.resource_generation) AND (x.route_generation = i.route_generation) AND (x.route_generation = i.target_generation))))) OR ((i.resource_kind = 'tunnel'::text) AND (EXISTS ( SELECT 1
           FROM (paperboat.tunnels t
             JOIN paperboat.tunnel_routes r ON ((r.tunnel_id = t.id)))
          WHERE ((t.id = i.resource_id) AND (t.account_id = i.owner_account_id) AND (t.generation = i.resource_generation) AND (t.desired_state = 'active'::text) AND (t.deleted_at IS NULL) AND ((t.expires_at IS NULL) OR (t.expires_at > now())) AND (r.id = i.route_id) AND (r.generation = i.route_generation) AND (r.generation = i.target_generation) AND (r.desired_state = 'active'::text) AND (r.deleted_at IS NULL) AND (t.created_by_host_id = i.native_machine_id)))))));


--
-- Name: lazy_access_policies; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.lazy_access_policies (
    id text NOT NULL,
    hostname text NOT NULL,
    account_id text NOT NULL,
    environment_id text NOT NULL,
    machine_id text NOT NULL,
    installation_generation bigint NOT NULL,
    generation bigint NOT NULL,
    target_scheme text NOT NULL,
    target_address text NOT NULL,
    access_mode text NOT NULL,
    ownership_mode text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    deleted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    authorization_source text DEFAULT 'explicit'::text NOT NULL,
    CONSTRAINT lazy_access_policies_access_mode_check CHECK ((access_mode = ANY (ARRAY['private'::text, 'team'::text]))),
    CONSTRAINT lazy_access_policies_authorization_source_check CHECK ((authorization_source = ANY (ARRAY['explicit'::text, 'device_service'::text]))),
    CONSTRAINT lazy_access_policies_generation_check CHECK ((generation > 0)),
    CONSTRAINT lazy_access_policies_hostname_check CHECK (((hostname = lower(hostname)) AND (hostname !~ '[:/\\\\[:cntrl:]]'::text))),
    CONSTRAINT lazy_access_policies_installation_generation_check CHECK ((installation_generation > 0)),
    CONSTRAINT lazy_access_policies_ownership_mode_check CHECK ((ownership_mode = 'persistent_port'::text)),
    CONSTRAINT lazy_access_policies_target_address_check CHECK (((octet_length(target_address) >= 1) AND (octet_length(target_address) <= 512))),
    CONSTRAINT lazy_access_policies_target_scheme_check CHECK ((target_scheme = ANY (ARRAY['http'::text, 'https'::text, 'h2c'::text])))
);


--
-- Name: lazy_activation_waiters; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.lazy_activation_waiters (
    waiter_id text NOT NULL,
    activation_id text NOT NULL,
    expires_at timestamp with time zone NOT NULL
);


--
-- Name: lazy_activations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.lazy_activations (
    policy_id text NOT NULL,
    policy_generation bigint NOT NULL,
    boot_id text NOT NULL,
    activation_id text NOT NULL,
    deadline timestamp with time zone NOT NULL,
    preview_id text,
    state text NOT NULL,
    error_code text DEFAULT ''::text NOT NULL,
    cooldown_until timestamp with time zone NOT NULL,
    CONSTRAINT lazy_activations_policy_generation_check CHECK ((policy_generation > 0)),
    CONSTRAINT lazy_activations_state_check CHECK ((state = ANY (ARRAY['running'::text, 'ready'::text, 'failed'::text])))
);


--
-- Name: lazy_environment_identities; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.lazy_environment_identities (
    environment_id text NOT NULL,
    environment_label text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT lazy_environment_identities_environment_label_check CHECK ((environment_label ~ '^[a-z0-9]{16}$'::text))
);


--
-- Name: lazy_runtime_owners; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.lazy_runtime_owners (
    machine_id text NOT NULL,
    account_id text NOT NULL,
    installation_generation bigint NOT NULL,
    boot_id text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT lazy_runtime_owners_boot_id_check CHECK (((length(boot_id) >= 16) AND (length(boot_id) <= 64))),
    CONSTRAINT lazy_runtime_owners_installation_generation_check CHECK ((installation_generation > 0))
);


--
-- Name: machine_control_renewals; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.machine_control_renewals (
    operation_id text NOT NULL,
    machine_id text NOT NULL,
    installation_generation bigint NOT NULL,
    credential_jti text NOT NULL,
    issued_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    session_generation bigint,
    superseded_at timestamp with time zone,
    CONSTRAINT machine_control_renewals_check CHECK ((expires_at > issued_at)),
    CONSTRAINT machine_control_renewals_installation_generation_check CHECK ((installation_generation > 0)),
    CONSTRAINT machine_control_renewals_operation_id_check CHECK (((length(operation_id) >= 8) AND (length(operation_id) <= 128))),
    CONSTRAINT machine_control_renewals_session_generation_positive CHECK (((session_generation IS NULL) OR (session_generation > 0)))
);


--
-- Name: machine_control_sessions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.machine_control_sessions (
    machine_id text NOT NULL,
    installation_generation bigint NOT NULL,
    session_generation bigint NOT NULL,
    operation_id text NOT NULL,
    credential_jti text NOT NULL,
    issued_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT machine_control_sessions_check CHECK ((expires_at > issued_at)),
    CONSTRAINT machine_control_sessions_installation_generation_check CHECK ((installation_generation > 0)),
    CONSTRAINT machine_control_sessions_session_generation_check CHECK ((session_generation > 0))
);


--
-- Name: machine_ssh_host_key_owners; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.machine_ssh_host_key_owners (
    fingerprint bytea NOT NULL,
    user_machine_id text NOT NULL,
    algorithm text NOT NULL,
    public_key text NOT NULL,
    first_observed_at timestamp with time zone NOT NULL,
    CONSTRAINT machine_ssh_host_key_owners_algorithm_check CHECK ((algorithm = ANY (ARRAY['ssh-ed25519'::text, 'ecdsa-sha2-nistp256'::text, 'ecdsa-sha2-nistp384'::text, 'ecdsa-sha2-nistp521'::text, 'ssh-rsa'::text]))),
    CONSTRAINT machine_ssh_host_key_owners_fingerprint_check CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT machine_ssh_host_key_owners_public_key_check CHECK (((octet_length(public_key) >= 80) AND (octet_length(public_key) <= 8192)))
);


--
-- Name: machine_ssh_host_key_sets; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.machine_ssh_host_key_sets (
    id text NOT NULL,
    user_machine_id text NOT NULL,
    machine_generation bigint NOT NULL,
    observation_generation bigint NOT NULL,
    set_fingerprint bytea NOT NULL,
    state text NOT NULL,
    reconciliation_version bigint NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    promoted_at timestamp with time zone,
    rejected_at timestamp with time zone,
    rejection_reason text,
    CONSTRAINT machine_ssh_host_key_sets_check CHECK (((state = ANY (ARRAY['active'::text, 'superseded'::text])) = (promoted_at IS NOT NULL))),
    CONSTRAINT machine_ssh_host_key_sets_check1 CHECK (((state = 'rejected'::text) = (rejected_at IS NOT NULL))),
    CONSTRAINT machine_ssh_host_key_sets_check2 CHECK (((rejected_at IS NULL) = (rejection_reason IS NULL))),
    CONSTRAINT machine_ssh_host_key_sets_check3 CHECK (((promoted_at IS NULL) OR (promoted_at >= observed_at))),
    CONSTRAINT machine_ssh_host_key_sets_check4 CHECK (((rejected_at IS NULL) OR (rejected_at >= observed_at))),
    CONSTRAINT machine_ssh_host_key_sets_id_check CHECK ((id ~ '^sshks_[A-Za-z0-9_-]{16,128}$'::text)),
    CONSTRAINT machine_ssh_host_key_sets_machine_generation_check CHECK ((machine_generation > 0)),
    CONSTRAINT machine_ssh_host_key_sets_observation_generation_check CHECK ((observation_generation > 0)),
    CONSTRAINT machine_ssh_host_key_sets_reconciliation_version_check CHECK ((reconciliation_version > 0)),
    CONSTRAINT machine_ssh_host_key_sets_rejection_reason_check CHECK (((rejection_reason IS NULL) OR (rejection_reason = ANY (ARRAY['operator_rejected'::text, 'machine_reenrolled'::text, 'account_revoked'::text, 'key_conflict'::text])))),
    CONSTRAINT machine_ssh_host_key_sets_set_fingerprint_check CHECK ((octet_length(set_fingerprint) = 32)),
    CONSTRAINT machine_ssh_host_key_sets_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'active'::text, 'superseded'::text, 'rejected'::text])))
);


--
-- Name: machine_ssh_host_keys; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.machine_ssh_host_keys (
    set_id text NOT NULL,
    user_machine_id text NOT NULL,
    fingerprint bytea NOT NULL,
    ordinal smallint NOT NULL,
    CONSTRAINT machine_ssh_host_keys_ordinal_check CHECK (((ordinal >= 0) AND (ordinal < 16)))
);


--
-- Name: machine_ssh_targets; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.machine_ssh_targets (
    user_machine_id text NOT NULL,
    machine_generation bigint NOT NULL,
    os_user text NOT NULL,
    target_port integer DEFAULT 22 NOT NULL,
    reconciliation_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT machine_ssh_targets_check CHECK ((updated_at >= created_at)),
    CONSTRAINT machine_ssh_targets_machine_generation_check CHECK ((machine_generation > 0)),
    CONSTRAINT machine_ssh_targets_os_user_check CHECK ((((octet_length(os_user) >= 1) AND (octet_length(os_user) <= 255)) AND (os_user = btrim(os_user)) AND (os_user !~ '[[:cntrl:][:space:]@]'::text) AND ("left"(os_user, 1) <> '-'::text))),
    CONSTRAINT machine_ssh_targets_reconciliation_version_check CHECK ((reconciliation_version > 0)),
    CONSTRAINT machine_ssh_targets_target_port_check CHECK (((target_port >= 1) AND (target_port <= 65535)))
);


--
-- Name: managed_node_authority; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.managed_node_authority (
    node_id text NOT NULL,
    region_id text NOT NULL,
    runtime_hash bytea NOT NULL,
    runtime_ciphertext bytea NOT NULL,
    config jsonb NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    retired_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT managed_node_authority_revision_check CHECK ((revision > 0)),
    CONSTRAINT managed_node_authority_runtime_hash_check CHECK ((octet_length(runtime_hash) = 32))
);


--
-- Name: managed_ssh_client_keys; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.managed_ssh_client_keys (
    fingerprint bytea NOT NULL,
    user_id text NOT NULL,
    cli_client_session_id text NOT NULL,
    algorithm text NOT NULL,
    public_key text NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    reconciliation_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    revocation_reason text,
    CONSTRAINT managed_ssh_client_keys_algorithm_check CHECK ((algorithm = 'ssh-ed25519'::text)),
    CONSTRAINT managed_ssh_client_keys_check CHECK (((state = 'active'::text) = (revoked_at IS NULL))),
    CONSTRAINT managed_ssh_client_keys_check1 CHECK (((revoked_at IS NULL) = (revocation_reason IS NULL))),
    CONSTRAINT managed_ssh_client_keys_check2 CHECK (((revoked_at IS NULL) OR (revoked_at >= created_at))),
    CONSTRAINT managed_ssh_client_keys_fingerprint_check CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT managed_ssh_client_keys_public_key_check CHECK (((octet_length(public_key) >= 80) AND (octet_length(public_key) <= 256))),
    CONSTRAINT managed_ssh_client_keys_reconciliation_version_check CHECK ((reconciliation_version > 0)),
    CONSTRAINT managed_ssh_client_keys_revocation_reason_check CHECK (((revocation_reason IS NULL) OR (revocation_reason = ANY (ARRAY['client_revoked'::text, 'client_logout'::text, 'account_revoked'::text, 'key_rotated'::text, 'key_compromise'::text])))),
    CONSTRAINT managed_ssh_client_keys_state_check CHECK ((state = ANY (ARRAY['active'::text, 'revoked'::text])))
);


--
-- Name: managed_ssh_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.managed_ssh_operations (
    operation_id text NOT NULL,
    user_id text NOT NULL,
    operation_kind text NOT NULL,
    request_hash bytea NOT NULL,
    resource_id text NOT NULL,
    result_revision bigint NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT managed_ssh_operations_operation_id_check CHECK ((operation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,255}$'::text)),
    CONSTRAINT managed_ssh_operations_operation_kind_check CHECK ((operation_kind = ANY (ARRAY['client_key_register'::text, 'client_key_revoke'::text, 'target_register'::text, 'target_update'::text, 'host_keys_observe'::text, 'host_keys_promote'::text]))),
    CONSTRAINT managed_ssh_operations_request_hash_check CHECK ((octet_length(request_hash) = 32)),
    CONSTRAINT managed_ssh_operations_resource_id_check CHECK (((octet_length(resource_id) >= 1) AND (octet_length(resource_id) <= 256))),
    CONSTRAINT managed_ssh_operations_result_revision_check CHECK ((result_revision > 0))
);


--
-- Name: operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.operations (
    id text NOT NULL,
    account_id text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash bytea NOT NULL,
    operation_type text NOT NULL,
    resource_kind text NOT NULL,
    resource_id text,
    phase text DEFAULT 'validating'::text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    progress smallint DEFAULT 0 NOT NULL,
    retrying boolean DEFAULT false NOT NULL,
    next_retry_at timestamp with time zone,
    error_code text,
    outcome text DEFAULT 'unchanged'::text NOT NULL,
    result_reference text,
    correlation_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT operations_check CHECK (((state = ANY (ARRAY['succeeded'::text, 'failed'::text, 'cancelled'::text])) = (completed_at IS NOT NULL))),
    CONSTRAINT operations_idempotency_key_check CHECK (((length(TRIM(BOTH FROM idempotency_key)) >= 1) AND (length(TRIM(BOTH FROM idempotency_key)) <= 256))),
    CONSTRAINT operations_operation_type_check CHECK (((length(TRIM(BOTH FROM operation_type)) >= 1) AND (length(TRIM(BOTH FROM operation_type)) <= 80))),
    CONSTRAINT operations_outcome_check CHECK ((outcome = ANY (ARRAY['unchanged'::text, 'changed'::text, 'uncertain'::text]))),
    CONSTRAINT operations_phase_check CHECK ((phase = ANY (ARRAY['validating'::text, 'persisting'::text, 'waiting_for_dns'::text, 'issuing_certificate'::text, 'installing_service'::text, 'connecting'::text, 'checking_origin'::text, 'draining'::text, 'rolling_back'::text, 'ready'::text, 'failed'::text]))),
    CONSTRAINT operations_progress_check CHECK (((progress >= 0) AND (progress <= 100))),
    CONSTRAINT operations_request_hash_check CHECK ((octet_length(request_hash) = 32)),
    CONSTRAINT operations_resource_kind_check CHECK ((resource_kind = ANY (ARRAY['preview_lease'::text, 'tunnel'::text, 'route'::text, 'domain_binding'::text, 'connector'::text]))),
    CONSTRAINT operations_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'cancelled'::text, 'uncertain'::text])))
);


--
-- Name: operator_actions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.operator_actions (
    id text NOT NULL,
    actor_user_id text NOT NULL,
    idempotency_key text NOT NULL,
    kind text NOT NULL,
    target text NOT NULL,
    request_hash bytea NOT NULL,
    result jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT operator_actions_idempotency_key_check CHECK (((length(TRIM(BOTH FROM idempotency_key)) >= 8) AND (length(TRIM(BOTH FROM idempotency_key)) <= 256))),
    CONSTRAINT operator_actions_kind_check CHECK ((kind = ANY (ARRAY['account_ban'::text, 'account_unban'::text, 'team_ban'::text, 'team_unban'::text, 'relay_drain'::text, 'relay_resume'::text, 'tunnel_disable'::text, 'tunnel_enable'::text]))),
    CONSTRAINT operator_actions_request_hash_check CHECK ((octet_length(request_hash) = 32)),
    CONSTRAINT operator_actions_target_check CHECK (((length(TRIM(BOTH FROM target)) >= 1) AND (length(TRIM(BOTH FROM target)) <= 256)))
);


--
-- Name: operator_authentication_challenges; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.operator_authentication_challenges (
    session_id text NOT NULL,
    state_hash text NOT NULL,
    code_verifier text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: operator_infrastructure_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.operator_infrastructure_operations (
    actor_user_id text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash bytea NOT NULL,
    result jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT operator_infrastructure_operations_request_hash_check CHECK ((octet_length(request_hash) = 32))
);


--
-- Name: operator_regions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.operator_regions (
    id text NOT NULL,
    display_name text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT operator_regions_display_name_check CHECK (((length(TRIM(BOTH FROM display_name)) >= 1) AND (length(TRIM(BOTH FROM display_name)) <= 80))),
    CONSTRAINT operator_regions_id_check CHECK ((id ~ '^[a-z][a-z0-9_-]{0,62}$'::text)),
    CONSTRAINT operator_regions_revision_check CHECK ((revision > 0))
);


--
-- Name: peer_endpoint_certificate_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.peer_endpoint_certificate_operations (
    operation_id text NOT NULL,
    user_id text NOT NULL,
    request_hash bytea NOT NULL,
    certificate_fingerprint bytea NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT peer_endpoint_certificate_operations_operation_id_check CHECK ((operation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,255}$'::text)),
    CONSTRAINT peer_endpoint_certificate_operations_request_hash_check CHECK ((octet_length(request_hash) = 32))
);


--
-- Name: peer_endpoint_certificate_revocations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.peer_endpoint_certificate_revocations (
    operation_id text NOT NULL,
    user_id text NOT NULL,
    certificate_fingerprint bytea NOT NULL,
    serial bigint NOT NULL,
    reason text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT peer_endpoint_certificate_revocations_operation_id_check CHECK ((operation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,255}$'::text)),
    CONSTRAINT peer_endpoint_certificate_revocations_reason_check CHECK ((reason = ANY (ARRAY['endpoint_removed'::text, 'device_removed'::text, 'account_revoked'::text, 'key_compromise'::text]))),
    CONSTRAINT peer_endpoint_certificate_revocations_serial_check CHECK ((serial > 0))
);


--
-- Name: peer_endpoint_certificates; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.peer_endpoint_certificates (
    fingerprint bytea NOT NULL,
    user_id text NOT NULL,
    endpoint_id text NOT NULL,
    role text NOT NULL,
    generation bigint NOT NULL,
    serial bigint NOT NULL,
    certificate bytea NOT NULL,
    quic_public_key bytea NOT NULL,
    issued_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    revocation_reason text,
    key_id text NOT NULL,
    CONSTRAINT peer_endpoint_certificates_certificate_check CHECK (((octet_length(certificate) >= 140) AND (octet_length(certificate) <= 394))),
    CONSTRAINT peer_endpoint_certificates_check CHECK ((expires_at > issued_at)),
    CONSTRAINT peer_endpoint_certificates_check1 CHECK (((revoked_at IS NULL) = (revocation_reason IS NULL))),
    CONSTRAINT peer_endpoint_certificates_check2 CHECK (((revoked_at IS NULL) OR (revoked_at >= created_at))),
    CONSTRAINT peer_endpoint_certificates_endpoint_id_check CHECK ((endpoint_id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$'::text)),
    CONSTRAINT peer_endpoint_certificates_fingerprint_check CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT peer_endpoint_certificates_generation_check CHECK ((generation > 0)),
    CONSTRAINT peer_endpoint_certificates_quic_public_key_check CHECK ((octet_length(quic_public_key) = 32)),
    CONSTRAINT peer_endpoint_certificates_revocation_reason_check CHECK (((revocation_reason IS NULL) OR (revocation_reason = ANY (ARRAY['endpoint_replaced'::text, 'endpoint_removed'::text, 'device_removed'::text, 'account_revoked'::text, 'key_compromise'::text, 'certificate_superseded'::text])))),
    CONSTRAINT peer_endpoint_certificates_role_check CHECK ((role = ANY (ARRAY['cli'::text, 'machine'::text]))),
    CONSTRAINT peer_endpoint_certificates_serial_check CHECK ((serial > 0))
);


--
-- Name: peer_endpoint_enrollment_requests; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.peer_endpoint_enrollment_requests (
    id text NOT NULL,
    operation_key text NOT NULL,
    request_hash bytea NOT NULL,
    user_id text NOT NULL,
    endpoint_id text NOT NULL,
    generation bigint NOT NULL,
    role text DEFAULT 'machine'::text NOT NULL,
    quic_public_key bytea NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    certificate_fingerprint bytea,
    created_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    fulfilled_at timestamp with time zone,
    CONSTRAINT peer_endpoint_enrollment_requests_check CHECK ((expires_at > created_at)),
    CONSTRAINT peer_endpoint_enrollment_requests_check1 CHECK (((state = 'fulfilled'::text) = ((certificate_fingerprint IS NOT NULL) AND (fulfilled_at IS NOT NULL)))),
    CONSTRAINT peer_endpoint_enrollment_requests_generation_check CHECK ((generation > 0)),
    CONSTRAINT peer_endpoint_enrollment_requests_id_check CHECK ((id ~ '^per_[A-Za-z0-9_-]{16,128}$'::text)),
    CONSTRAINT peer_endpoint_enrollment_requests_operation_key_check CHECK (((length(operation_key) >= 8) AND (length(operation_key) <= 128))),
    CONSTRAINT peer_endpoint_enrollment_requests_quic_public_key_check CHECK ((octet_length(quic_public_key) = 32)),
    CONSTRAINT peer_endpoint_enrollment_requests_request_hash_check CHECK ((octet_length(request_hash) = 32)),
    CONSTRAINT peer_endpoint_enrollment_requests_role_check CHECK ((role = ANY (ARRAY['machine'::text, 'cli'::text]))),
    CONSTRAINT peer_endpoint_enrollment_requests_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'fulfilled'::text, 'expired'::text, 'revoked'::text])))
);


--
-- Name: peer_network_identities; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.peer_network_identities (
    user_id text NOT NULL,
    endpoint_id text NOT NULL,
    role text NOT NULL,
    machine_id text,
    endpoint_generation bigint NOT NULL,
    machine_generation bigint NOT NULL,
    key_generation bigint NOT NULL,
    wireguard_public_key bytea NOT NULL,
    quic_certificate_fingerprint bytea NOT NULL,
    virtual_address inet NOT NULL,
    config_generation bigint DEFAULT 0 NOT NULL,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    disco_public_key bytea,
    relay_revocation_generation bigint DEFAULT 0 NOT NULL,
    CONSTRAINT peer_network_identities_check CHECK ((((role = 'cli'::text) AND (machine_id IS NULL) AND (machine_generation = 0)) OR ((role = 'machine'::text) AND (machine_id = endpoint_id) AND (machine_generation > 0)))),
    CONSTRAINT peer_network_identities_config_generation_check CHECK ((config_generation >= 0)),
    CONSTRAINT peer_network_identities_disco_public_key_check CHECK (((disco_public_key IS NULL) OR (octet_length(disco_public_key) = 32))),
    CONSTRAINT peer_network_identities_endpoint_generation_check CHECK ((endpoint_generation > 0)),
    CONSTRAINT peer_network_identities_key_generation_check CHECK ((key_generation > 0)),
    CONSTRAINT peer_network_identities_machine_generation_check CHECK ((machine_generation >= 0)),
    CONSTRAINT peer_network_identities_relay_revocation_generation_check CHECK ((relay_revocation_generation >= 0)),
    CONSTRAINT peer_network_identities_role_check CHECK ((role = ANY (ARRAY['cli'::text, 'machine'::text]))),
    CONSTRAINT peer_network_identities_wireguard_public_key_check CHECK ((octet_length(wireguard_public_key) = 32))
);


--
-- Name: peer_network_registration_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.peer_network_registration_operations (
    operation_id text NOT NULL,
    user_id text NOT NULL,
    endpoint_id text NOT NULL,
    request_hash bytea NOT NULL,
    key_generation bigint NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT peer_network_registration_operations_key_generation_check CHECK ((key_generation > 0)),
    CONSTRAINT peer_network_registration_operations_operation_id_check CHECK (((length(operation_id) >= 8) AND (length(operation_id) <= 128))),
    CONSTRAINT peer_network_registration_operations_request_hash_check CHECK ((octet_length(request_hash) = 32))
);


--
-- Name: peer_network_virtual_address_seq; Type: SEQUENCE; Schema: paperboat; Owner: -
--

CREATE SEQUENCE paperboat.peer_network_virtual_address_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    MAXVALUE 9007199254740991
    CACHE 1;


--
-- Name: polar_events; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.polar_events (
    id text NOT NULL,
    provider_event_id text NOT NULL,
    event_type text NOT NULL,
    processed_state text NOT NULL,
    payload jsonb NOT NULL,
    processed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: preview_carrier_attachment_outbox; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.preview_carrier_attachment_outbox (
    account_id text NOT NULL,
    operation_id text NOT NULL,
    attachment_generation bigint NOT NULL,
    action text NOT NULL,
    binding jsonb NOT NULL,
    config_content_hash text NOT NULL,
    edge_endpoints text[] NOT NULL,
    endpoint text NOT NULL,
    access_mode text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    last_error_code text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    delivered_at timestamp with time zone,
    CONSTRAINT preview_carrier_attachment_outbox_access_mode_check CHECK ((access_mode = ANY (ARRAY['public'::text, 'private'::text, 'team'::text]))),
    CONSTRAINT preview_carrier_attachment_outbox_action_check CHECK ((action = ANY (ARRAY['admit'::text, 'detach'::text]))),
    CONSTRAINT preview_carrier_attachment_outbox_attachment_generation_check CHECK ((attachment_generation > 0)),
    CONSTRAINT preview_carrier_attachment_outbox_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT preview_carrier_attachment_outbox_binding_check CHECK ((jsonb_typeof(binding) = 'object'::text)),
    CONSTRAINT preview_carrier_attachment_outbox_check CHECK (((state = 'delivered'::text) = (delivered_at IS NOT NULL))),
    CONSTRAINT preview_carrier_attachment_outbox_config_content_hash_check CHECK ((config_content_hash ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT preview_carrier_attachment_outbox_edge_endpoints_check CHECK ((cardinality(edge_endpoints) = 2)),
    CONSTRAINT preview_carrier_attachment_outbox_edge_process_epoch_check CHECK (((binding ? 'edge_process_epoch'::text) AND ((length((binding ->> 'edge_process_epoch'::text)) >= 8) AND (length((binding ->> 'edge_process_epoch'::text)) <= 128)) AND ((binding ->> 'edge_process_epoch'::text) ~ '^[A-Za-z0-9_-]+$'::text))),
    CONSTRAINT preview_carrier_attachment_outbox_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'in_flight'::text, 'delivered'::text, 'failed'::text])))
);


--
-- Name: TABLE preview_carrier_attachment_outbox; Type: COMMENT; Schema: paperboat; Owner: -
--

COMMENT ON TABLE paperboat.preview_carrier_attachment_outbox IS 'Replayable edge admission/detach intents containing safe binding metadata only.';


--
-- Name: preview_domains; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.preview_domains (
    id text NOT NULL,
    account_id text NOT NULL,
    preview_id text NOT NULL,
    preview_generation bigint NOT NULL,
    hostname text NOT NULL,
    match_type text NOT NULL,
    ownership_challenge_reference text NOT NULL,
    ownership_state text DEFAULT 'pending'::text NOT NULL,
    dns_target text NOT NULL,
    observed_records jsonb DEFAULT '[]'::jsonb NOT NULL,
    dns_provider text DEFAULT 'generic'::text NOT NULL,
    expected_records jsonb DEFAULT '[]'::jsonb NOT NULL,
    dns_last_checked_at timestamp with time zone,
    dns_next_check_at timestamp with time zone DEFAULT now() NOT NULL,
    dns_ttl_seconds integer,
    verification_attempts integer DEFAULT 0 NOT NULL,
    certificate_strategy text DEFAULT 'managed'::text NOT NULL,
    certificate_reference text,
    certificate_state text DEFAULT 'pending'::text NOT NULL,
    certificate_expires_at timestamp with time zone,
    certificate_renewal_attempted_at timestamp with time zone,
    certificate_failure_code text,
    caa_state text DEFAULT 'unknown'::text NOT NULL,
    conflict_state text DEFAULT 'clear'::text NOT NULL,
    last_verified_at timestamp with time zone,
    generation bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    quarantine_until timestamp with time zone,
    CONSTRAINT preview_domains_caa_state_check CHECK ((caa_state = ANY (ARRAY['unknown'::text, 'ready'::text, 'blocked'::text, 'not_applicable'::text]))),
    CONSTRAINT preview_domains_certificate_state_check CHECK ((certificate_state = ANY (ARRAY['pending'::text, 'issuing'::text, 'ready'::text, 'renewing'::text, 'failed'::text, 'expired'::text, 'revoked'::text, 'not_applicable'::text]))),
    CONSTRAINT preview_domains_certificate_strategy_check CHECK ((certificate_strategy = ANY (ARRAY['managed'::text, 'provided_reference'::text, 'on_demand_leaf'::text, 'none'::text]))),
    CONSTRAINT preview_domains_check CHECK (((preview_generation > 0) AND (generation > 0))),
    CONSTRAINT preview_domains_check1 CHECK ((((match_type = 'exact'::text) AND (hostname !~ '^[*][.]'::text)) OR ((match_type = 'one_label_wildcard'::text) AND (hostname ~ '^[*][.][a-z0-9]'::text)))),
    CONSTRAINT preview_domains_check2 CHECK (((certificate_strategy = 'none'::text) = (certificate_state = 'not_applicable'::text))),
    CONSTRAINT preview_domains_check3 CHECK (((conflict_state = 'quarantined'::text) = (quarantine_until IS NOT NULL))),
    CONSTRAINT preview_domains_check4 CHECK (((deleted_at IS NULL) OR ((conflict_state = 'quarantined'::text) AND (quarantine_until IS NOT NULL)))),
    CONSTRAINT preview_domains_conflict_state_check CHECK ((conflict_state = ANY (ARRAY['clear'::text, 'conflicted'::text, 'quarantined'::text]))),
    CONSTRAINT preview_domains_dns_provider_check CHECK ((dns_provider = ANY (ARRAY['generic'::text, 'cloudflare'::text, 'route53'::text, 'google_cloud_dns'::text, 'digitalocean'::text, 'namecheap'::text]))),
    CONSTRAINT preview_domains_dns_ttl_seconds_check CHECK (((dns_ttl_seconds IS NULL) OR ((dns_ttl_seconds >= 30) AND (dns_ttl_seconds <= 86400)))),
    CONSTRAINT preview_domains_expected_records_check CHECK ((jsonb_typeof(expected_records) = 'array'::text)),
    CONSTRAINT preview_domains_hostname_check CHECK (((hostname = btrim(hostname)) AND (hostname = lower(hostname)) AND (hostname !~ '[.]$'::text))),
    CONSTRAINT preview_domains_observed_records_check CHECK ((jsonb_typeof(observed_records) = 'array'::text)),
    CONSTRAINT preview_domains_ownership_state_check CHECK ((ownership_state = ANY (ARRAY['pending'::text, 'verified'::text, 'failed'::text, 'expired'::text, 'revoked'::text]))),
    CONSTRAINT preview_domains_verification_attempts_check CHECK ((verification_attempts >= 0))
);


--
-- Name: preview_lease_create_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.preview_lease_create_operations (
    account_id text NOT NULL,
    preview_id text NOT NULL,
    operation_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: provider_events; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.provider_events (
    id text NOT NULL,
    provider text NOT NULL,
    provider_event_id text NOT NULL,
    event_type text NOT NULL,
    processed_state text NOT NULL,
    payload jsonb NOT NULL,
    processed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: provider_route_cleanup_outbox; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.provider_route_cleanup_outbox (
    id text NOT NULL,
    project_id text NOT NULL,
    action text NOT NULL,
    reason text NOT NULL,
    propagated_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: release_authority_bundles; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.release_authority_bundles (
    id text NOT NULL,
    release_id text NOT NULL,
    version text NOT NULL,
    platform text NOT NULL,
    architecture text NOT NULL,
    action text NOT NULL,
    policy_revision bigint NOT NULL,
    payload jsonb NOT NULL,
    payload_hash bytea NOT NULL,
    signatures jsonb NOT NULL,
    issued_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    authority_request_id text NOT NULL,
    imported_by_user_id text NOT NULL,
    imported_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT release_authority_bundles_action_check CHECK ((action = ANY (ARRAY['promote'::text, 'pause'::text, 'quarantine'::text, 'revoke'::text]))),
    CONSTRAINT release_authority_bundles_architecture_check CHECK ((architecture = ANY (ARRAY['amd64'::text, 'arm64'::text]))),
    CONSTRAINT release_authority_bundles_check CHECK ((expires_at > issued_at)),
    CONSTRAINT release_authority_bundles_platform_check CHECK ((platform = ANY (ARRAY['darwin'::text, 'linux'::text, 'windows'::text]))),
    CONSTRAINT release_authority_bundles_policy_revision_check CHECK ((policy_revision > 0))
);


--
-- Name: release_authority_import_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.release_authority_import_operations (
    actor_user_id text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash bytea NOT NULL,
    bundle_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT release_authority_import_operations_idempotency_key_check CHECK (((length(TRIM(BOTH FROM idempotency_key)) >= 8) AND (length(TRIM(BOTH FROM idempotency_key)) <= 128)))
);


--
-- Name: release_authority_requests; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.release_authority_requests (
    id text NOT NULL,
    action text NOT NULL,
    release_id text NOT NULL,
    version text NOT NULL,
    platform text NOT NULL,
    architecture text NOT NULL,
    policy_revision bigint NOT NULL,
    rollout_percentage integer NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    requested_by_user_id text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash bytea NOT NULL,
    fulfilled_bundle_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    fulfilled_at timestamp with time zone,
    CONSTRAINT release_authority_requests_action_check CHECK ((action = ANY (ARRAY['promote'::text, 'pause'::text, 'quarantine'::text, 'revoke'::text]))),
    CONSTRAINT release_authority_requests_policy_revision_check CHECK ((policy_revision > 0)),
    CONSTRAINT release_authority_requests_rollout_percentage_check CHECK (((rollout_percentage >= 0) AND (rollout_percentage <= 100))),
    CONSTRAINT release_authority_requests_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'fulfilled'::text, 'cancelled'::text])))
);


--
-- Name: selfhost_challenges; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.selfhost_challenges (
    hash bytea NOT NULL,
    public_key bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT selfhost_challenges_public_key_check CHECK ((octet_length(public_key) = 32))
);


--
-- Name: selfhost_installations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.selfhost_installations (
    id text NOT NULL,
    node_id text NOT NULL,
    operator_public_key bytea NOT NULL,
    runtime_hash bytea NOT NULL,
    runtime_ciphertext bytea NOT NULL,
    name text NOT NULL,
    capability text NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT selfhost_installations_capability_check CHECK ((capability = ANY (ARRAY['relay'::text, 'tunnel'::text]))),
    CONSTRAINT selfhost_installations_name_check CHECK (((length(name) >= 1) AND (length(name) <= 80))),
    CONSTRAINT selfhost_installations_operator_public_key_check CHECK ((octet_length(operator_public_key) = 32)),
    CONSTRAINT selfhost_installations_runtime_hash_check CHECK ((octet_length(runtime_hash) = 32)),
    CONSTRAINT selfhost_installations_state_check CHECK ((state = ANY (ARRAY['active'::text, 'removed'::text])))
);


--
-- Name: selfhost_operator_nonces; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.selfhost_operator_nonces (
    installation_id text NOT NULL,
    nonce text NOT NULL,
    expires_at timestamp with time zone NOT NULL
);


--
-- Name: selfhost_pairing_codes; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.selfhost_pairing_codes (
    hash bytea NOT NULL,
    installation_id text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    redeemed_by text,
    redeemed_at timestamp with time zone,
    revoked_at timestamp with time zone
);


--
-- Name: selfhost_pairings; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.selfhost_pairings (
    installation_id text NOT NULL,
    account_id text NOT NULL,
    capability text NOT NULL,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT selfhost_pairings_capability_check CHECK ((capability = ANY (ARRAY['relay'::text, 'tunnel'::text])))
);


--
-- Name: selfhost_pool_policies; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.selfhost_pool_policies (
    account_id text NOT NULL,
    capability text NOT NULL,
    mode text NOT NULL,
    installation_ids text[] DEFAULT '{}'::text[] NOT NULL,
    CONSTRAINT selfhost_pool_policies_capability_check CHECK ((capability = ANY (ARRAY['relay'::text, 'tunnel'::text]))),
    CONSTRAINT selfhost_pool_policies_installation_ids_check CHECK ((cardinality(installation_ids) <= 32)),
    CONSTRAINT selfhost_pool_policies_mode_check CHECK ((mode = ANY (ARRAY['mixed'::text, 'self-hosted-only'::text])))
);


--
-- Name: sessions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.sessions (
    id text NOT NULL,
    user_id text NOT NULL,
    session_hash text NOT NULL,
    csrf_hash text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    rotated_at timestamp with time zone,
    revoked_at timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    operator_authenticated_at timestamp with time zone
);


--
-- Name: tunnel_domains; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_domains (
    id text NOT NULL,
    account_id text NOT NULL,
    tunnel_id text NOT NULL,
    route_id text NOT NULL,
    hostname text NOT NULL,
    match_type text NOT NULL,
    ownership_challenge_reference text NOT NULL,
    ownership_state text DEFAULT 'pending'::text NOT NULL,
    dns_target text NOT NULL,
    observed_records jsonb DEFAULT '[]'::jsonb NOT NULL,
    certificate_strategy text DEFAULT 'managed'::text NOT NULL,
    certificate_reference text,
    certificate_state text DEFAULT 'pending'::text NOT NULL,
    certificate_expires_at timestamp with time zone,
    certificate_renewal_attempted_at timestamp with time zone,
    certificate_failure_code text,
    caa_state text DEFAULT 'unknown'::text NOT NULL,
    conflict_state text DEFAULT 'clear'::text NOT NULL,
    last_verified_at timestamp with time zone,
    generation bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    dns_provider text DEFAULT 'generic'::text NOT NULL,
    expected_records jsonb DEFAULT '[]'::jsonb NOT NULL,
    dns_last_checked_at timestamp with time zone,
    dns_next_check_at timestamp with time zone DEFAULT now() NOT NULL,
    dns_ttl_seconds integer,
    verification_attempts integer DEFAULT 0 NOT NULL,
    quarantine_until timestamp with time zone,
    CONSTRAINT tunnel_domains_caa_state_check CHECK ((caa_state = ANY (ARRAY['unknown'::text, 'ready'::text, 'blocked'::text, 'not_applicable'::text]))),
    CONSTRAINT tunnel_domains_certificate_state_check CHECK ((certificate_state = ANY (ARRAY['pending'::text, 'issuing'::text, 'ready'::text, 'renewing'::text, 'failed'::text, 'expired'::text, 'revoked'::text, 'not_applicable'::text]))),
    CONSTRAINT tunnel_domains_certificate_strategy_check CHECK ((certificate_strategy = ANY (ARRAY['managed'::text, 'provided_reference'::text, 'on_demand_leaf'::text, 'none'::text]))),
    CONSTRAINT tunnel_domains_check CHECK ((((match_type = 'exact'::text) AND (hostname !~ '^[*][.]'::text)) OR ((match_type = 'one_label_wildcard'::text) AND (hostname ~ '^[*][.][a-z0-9]'::text)))),
    CONSTRAINT tunnel_domains_check1 CHECK (((certificate_strategy = 'none'::text) = (certificate_state = 'not_applicable'::text))),
    CONSTRAINT tunnel_domains_conflict_state_check CHECK ((conflict_state = ANY (ARRAY['clear'::text, 'conflicted'::text, 'quarantined'::text]))),
    CONSTRAINT tunnel_domains_dns_provider_check CHECK ((dns_provider = ANY (ARRAY['generic'::text, 'cloudflare'::text, 'route53'::text, 'google_cloud_dns'::text, 'digitalocean'::text, 'namecheap'::text]))),
    CONSTRAINT tunnel_domains_dns_ttl_check CHECK (((dns_ttl_seconds IS NULL) OR ((dns_ttl_seconds >= 30) AND (dns_ttl_seconds <= 86400)))),
    CONSTRAINT tunnel_domains_expected_records_check CHECK ((jsonb_typeof(expected_records) = 'array'::text)),
    CONSTRAINT tunnel_domains_generation_check CHECK ((generation > 0)),
    CONSTRAINT tunnel_domains_hostname_check CHECK (((hostname = lower(hostname)) AND (hostname !~ '[.]$'::text))),
    CONSTRAINT tunnel_domains_observed_records_check CHECK ((jsonb_typeof(observed_records) = 'array'::text)),
    CONSTRAINT tunnel_domains_ownership_state_check CHECK ((ownership_state = ANY (ARRAY['pending'::text, 'verified'::text, 'failed'::text, 'expired'::text, 'revoked'::text]))),
    CONSTRAINT tunnel_domains_quarantine_check CHECK (((conflict_state = 'quarantined'::text) = (quarantine_until IS NOT NULL))),
    CONSTRAINT tunnel_domains_verification_attempts_check CHECK ((verification_attempts >= 0))
);


--
-- Name: shared_443_names_v1; Type: VIEW; Schema: paperboat; Owner: -
--

CREATE VIEW paperboat.shared_443_names_v1 AS
 SELECT ('route:'::text || r.id) AS owner,
    (r.protocol = 'tls'::text) AS passthrough,
    lower(rtrim(COALESCE(r.match_hostname, ('*.'::text || r.wildcard_suffix), SUBSTRING(t.stable_endpoint FROM 9)), '.'::text)) AS hostname
   FROM (paperboat.tunnel_routes r
     JOIN paperboat.tunnels t ON ((t.id = r.tunnel_id)))
  WHERE ((r.protocol = ANY (ARRAY['http'::text, 'tls'::text])) AND (r.desired_state <> 'deleted'::text) AND (t.desired_state <> 'deleted'::text))
UNION ALL
 SELECT ('domain:'::text || d.id) AS owner,
    (r.protocol = 'tls'::text) AS passthrough,
    lower(rtrim(d.hostname, '.'::text)) AS hostname
   FROM ((paperboat.tunnel_domains d
     JOIN paperboat.tunnel_routes r ON ((r.id = d.route_id)))
     JOIN paperboat.tunnels t ON ((t.id = r.tunnel_id)))
  WHERE ((d.deleted_at IS NULL) AND (r.protocol = ANY (ARRAY['http'::text, 'tls'::text])) AND (r.desired_state <> 'deleted'::text) AND (t.desired_state <> 'deleted'::text))
UNION ALL
 SELECT ('preview:'::text || preview_leases.id) AS owner,
    false AS passthrough,
    lower(rtrim(SUBSTRING(preview_leases.endpoint FROM 9), '.'::text)) AS hostname
   FROM paperboat.preview_leases
  WHERE (preview_leases.terminal_state = 'active'::text)
UNION ALL
 SELECT ('preview-domain:'::text || d.id) AS owner,
    false AS passthrough,
    lower(rtrim(d.hostname, '.'::text)) AS hostname
   FROM (paperboat.preview_domains d
     JOIN paperboat.preview_leases p ON ((p.id = d.preview_id)))
  WHERE ((d.deleted_at IS NULL) AND (p.terminal_state = 'active'::text))
UNION ALL
 SELECT ('node:'::text || control_tunnel_nodes.id) AS owner,
    false AS passthrough,
    lower(rtrim(control_tunnel_nodes.endpoint_host, '.'::text)) AS hostname
   FROM paperboat.control_tunnel_nodes
  WHERE (control_tunnel_nodes.endpoint_host IS NOT NULL);


--
-- Name: stripe_events; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.stripe_events (
    id text NOT NULL,
    event_type text NOT NULL,
    subscription_id text NOT NULL,
    processed_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: support_reports; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.support_reports (
    id text NOT NULL,
    support_reference text NOT NULL,
    owner_id text NOT NULL,
    request_id text NOT NULL,
    component text NOT NULL,
    release text NOT NULL,
    stage text NOT NULL,
    code text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    sentry_event_id text NOT NULL,
    source_file text NOT NULL,
    source_function text NOT NULL,
    source_line integer NOT NULL,
    reporting text NOT NULL,
    CONSTRAINT support_reports_reporting_check CHECK ((reporting = ANY (ARRAY['disabled'::text, 'filtered'::text, 'unconfirmed'::text]))),
    CONSTRAINT support_reports_support_reference_check CHECK ((support_reference ~ '^pb-[0-9a-f]{32}$'::text))
);


--
-- Name: team_config_default_adoptions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_config_default_adoptions (
    team_id text NOT NULL,
    account_id text NOT NULL,
    default_version bigint NOT NULL,
    repository_id text NOT NULL,
    adopted_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT team_config_default_adoptions_default_version_check CHECK ((default_version > 0))
);


--
-- Name: team_config_defaults; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_config_defaults (
    team_id text NOT NULL,
    provider text NOT NULL,
    external_repository_id text NOT NULL,
    display_name text NOT NULL,
    branch text NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    updated_by text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT team_config_defaults_branch_check CHECK (((length(branch) >= 1) AND (length(branch) <= 255))),
    CONSTRAINT team_config_defaults_display_name_check CHECK (((length(display_name) >= 1) AND (length(display_name) <= 128))),
    CONSTRAINT team_config_defaults_external_repository_id_check CHECK (((length(external_repository_id) >= 1) AND (length(external_repository_id) <= 128))),
    CONSTRAINT team_config_defaults_provider_check CHECK (((length(provider) >= 1) AND (length(provider) <= 32))),
    CONSTRAINT team_config_defaults_version_check CHECK ((version > 0))
);


--
-- Name: team_inbox_policies; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_inbox_policies (
    account_id text NOT NULL,
    acceptance text DEFAULT 'manual'::text NOT NULL,
    receipt_email boolean DEFAULT false NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT team_inbox_policies_acceptance_check CHECK ((acceptance = ANY (ARRAY['manual'::text, 'automatic'::text]))),
    CONSTRAINT team_inbox_policies_generation_check CHECK ((generation > 0))
);


--
-- Name: team_inbox_receipt_outbox; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_inbox_receipt_outbox (
    request_id text NOT NULL,
    recipient_account text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    provider_message_id text,
    last_error_code text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT team_inbox_receipt_outbox_attempts_check CHECK (((attempts >= 0) AND (attempts <= 5))),
    CONSTRAINT team_inbox_receipt_outbox_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'delivering'::text, 'delivered'::text, 'failed'::text])))
);


--
-- Name: team_inbox_requests; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_inbox_requests (
    request_id text NOT NULL,
    operation_id text NOT NULL,
    team_id text NOT NULL,
    sender_account text NOT NULL,
    recipient_account text NOT NULL,
    source_machine_id text NOT NULL,
    destination_machine_id text NOT NULL,
    batch_id text NOT NULL,
    manifest_digest text NOT NULL,
    manifest jsonb NOT NULL,
    status text NOT NULL,
    decision_kind text NOT NULL,
    policy_generation bigint NOT NULL,
    decision_generation bigint DEFAULT 1 NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    decided_at timestamp with time zone,
    consumed_at timestamp with time zone,
    completed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT team_inbox_requests_check CHECK ((sender_account <> recipient_account)),
    CONSTRAINT team_inbox_requests_check1 CHECK ((source_machine_id <> destination_machine_id)),
    CONSTRAINT team_inbox_requests_decision_generation_check CHECK ((decision_generation > 0)),
    CONSTRAINT team_inbox_requests_decision_kind_check CHECK ((decision_kind = ANY (ARRAY['manual'::text, 'automatic'::text]))),
    CONSTRAINT team_inbox_requests_manifest_check CHECK (((jsonb_typeof(manifest) = 'array'::text) AND ((jsonb_array_length(manifest) >= 1) AND (jsonb_array_length(manifest) <= 10)) AND (octet_length((manifest)::text) <= 16384))),
    CONSTRAINT team_inbox_requests_manifest_digest_check CHECK ((manifest_digest ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT team_inbox_requests_policy_generation_check CHECK ((policy_generation > 0)),
    CONSTRAINT team_inbox_requests_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'approved'::text, 'declined'::text, 'expired'::text, 'revoked'::text, 'consumed'::text, 'completed'::text])))
);


--
-- Name: team_inbox_sender_policies; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_inbox_sender_policies (
    recipient_account text NOT NULL,
    sender_account text NOT NULL,
    acceptance text NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT team_inbox_sender_policies_acceptance_check CHECK ((acceptance = ANY (ARRAY['manual'::text, 'automatic'::text]))),
    CONSTRAINT team_inbox_sender_policies_check CHECK ((recipient_account <> sender_account)),
    CONSTRAINT team_inbox_sender_policies_generation_check CHECK ((generation > 0))
);


--
-- Name: team_invitations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_invitations (
    invitation_id text NOT NULL,
    team_id text NOT NULL,
    recipient_account text NOT NULL,
    created_by text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    accepted_at timestamp with time zone,
    cancelled_at timestamp with time zone,
    CONSTRAINT team_invitations_check CHECK (((accepted_at IS NULL) OR (cancelled_at IS NULL)))
);


--
-- Name: team_machine_grants; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_machine_grants (
    team_id text NOT NULL,
    resource_kind text DEFAULT 'machine'::text NOT NULL,
    machine_id text NOT NULL,
    audience text NOT NULL,
    account_id text,
    capabilities text[] NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    active boolean DEFAULT true NOT NULL,
    CONSTRAINT team_machine_grants_audience_check CHECK ((audience = ANY (ARRAY['all_members'::text, 'selected_member'::text]))),
    CONSTRAINT team_machine_grants_capabilities_check CHECK ((((cardinality(capabilities) >= 1) AND (cardinality(capabilities) <= 7)) AND (capabilities <@ ARRAY['terminal'::text, 'exec'::text, 'managed_ssh'::text, 'files'::text, 'preview_manage'::text, 'tunnel_manage'::text, 'private_access'::text]) AND (array_position(capabilities, NULL::text) IS NULL))),
    CONSTRAINT team_machine_grants_check CHECK ((((audience = 'all_members'::text) AND (account_id IS NULL)) OR ((audience = 'selected_member'::text) AND (account_id IS NOT NULL)))),
    CONSTRAINT team_machine_grants_generation_check CHECK (((generation >= 1) AND (generation <= '9007199254740991'::bigint))),
    CONSTRAINT team_machine_grants_resource_kind_check CHECK ((resource_kind = 'machine'::text))
);


--
-- Name: team_network_preferences; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_network_preferences (
    team_id text NOT NULL,
    revision bigint NOT NULL,
    device_suffix text,
    device_loopback_cidr text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT team_network_preferences_revision_check CHECK (((revision >= 1) AND (revision <= '9007199254740991'::bigint)))
);


--
-- Name: team_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_operations (
    account_id text NOT NULL,
    operation_id text NOT NULL,
    request_digest bytea NOT NULL,
    result bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT team_operations_request_digest_check CHECK ((octet_length(request_digest) = 32)),
    CONSTRAINT team_operations_result_check CHECK (((octet_length(result) >= 1) AND (octet_length(result) <= 1024)))
);


--
-- Name: team_receipt_smtp; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_receipt_smtp (
    team_id text NOT NULL,
    host text NOT NULL,
    port integer NOT NULL,
    username text NOT NULL,
    password_ciphertext bytea NOT NULL,
    from_address text NOT NULL,
    tls_mode text NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT team_receipt_smtp_generation_check CHECK ((generation > 0)),
    CONSTRAINT team_receipt_smtp_port_check CHECK ((port = ANY (ARRAY[465, 587]))),
    CONSTRAINT team_receipt_smtp_tls_mode_check CHECK ((tls_mode = ANY (ARRAY['implicit'::text, 'starttls'::text])))
);


--
-- Name: team_terminal_session_grants; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.team_terminal_session_grants (
    team_id text NOT NULL,
    resource_kind text DEFAULT 'terminal_session'::text NOT NULL,
    terminal_session_id text NOT NULL,
    audience text NOT NULL,
    account_id text,
    role text NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    active boolean DEFAULT true NOT NULL,
    CONSTRAINT team_terminal_session_grants_audience_check CHECK ((audience = ANY (ARRAY['all_members'::text, 'selected_member'::text]))),
    CONSTRAINT team_terminal_session_grants_check CHECK ((((audience = 'all_members'::text) AND (account_id IS NULL)) OR ((audience = 'selected_member'::text) AND (account_id IS NOT NULL)))),
    CONSTRAINT team_terminal_session_grants_generation_check CHECK (((generation >= 1) AND (generation <= '9007199254740991'::bigint))),
    CONSTRAINT team_terminal_session_grants_resource_kind_check CHECK ((resource_kind = 'terminal_session'::text)),
    CONSTRAINT team_terminal_session_grants_role_check CHECK ((role = ANY (ARRAY['viewer'::text, 'interactive'::text])))
);


--
-- Name: tunnel_certificate_edge_distributions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_certificate_edge_distributions (
    certificate_id text NOT NULL,
    edge_node_id text NOT NULL,
    edge_process_epoch text NOT NULL,
    edge_assignment_generation bigint NOT NULL,
    state text DEFAULT 'staged'::text NOT NULL,
    observed_certificate_generation bigint NOT NULL,
    observed_at timestamp with time zone,
    failure_code text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tunnel_certificate_edge_dist_observed_certificate_generat_check CHECK ((observed_certificate_generation > 0)),
    CONSTRAINT tunnel_certificate_edge_distri_edge_assignment_generation_check CHECK ((edge_assignment_generation > 0)),
    CONSTRAINT tunnel_certificate_edge_distributions_edge_process_epoch_check CHECK ((((length(TRIM(BOTH FROM edge_process_epoch)) >= 8) AND (length(TRIM(BOTH FROM edge_process_epoch)) <= 128)) AND (edge_process_epoch ~ '^[A-Za-z0-9_-]+$'::text))),
    CONSTRAINT tunnel_certificate_edge_distributions_state_check CHECK ((state = ANY (ARRAY['staged'::text, 'ready'::text, 'active'::text, 'retired'::text, 'revoked'::text, 'failed'::text])))
);


--
-- Name: tunnel_certificate_issuance_locks; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_certificate_issuance_locks (
    domain_id text NOT NULL,
    owner_id text NOT NULL,
    domain_generation bigint NOT NULL,
    lease_until timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tunnel_certificate_issuance_locks_check CHECK ((lease_until > updated_at)),
    CONSTRAINT tunnel_certificate_issuance_locks_domain_generation_check CHECK ((domain_generation > 0)),
    CONSTRAINT tunnel_certificate_issuance_locks_owner_id_check CHECK (((length(TRIM(BOTH FROM owner_id)) >= 3) AND (length(TRIM(BOTH FROM owner_id)) <= 128)))
);


--
-- Name: tunnel_certificate_records; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_certificate_records (
    id text NOT NULL,
    domain_id text,
    account_id text NOT NULL,
    tunnel_id text,
    hostname text NOT NULL,
    domain_generation bigint NOT NULL,
    certificate_generation bigint NOT NULL,
    strategy text NOT NULL,
    state text DEFAULT 'staged'::text NOT NULL,
    certificate_reference text NOT NULL,
    master_key_reference text NOT NULL,
    certificate_ciphertext bytea NOT NULL,
    private_key_ciphertext bytea NOT NULL,
    fingerprint bytea NOT NULL,
    issuer text NOT NULL,
    not_before timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    renewal_at timestamp with time zone NOT NULL,
    failure_code text,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    leaf_hostname text,
    target_kind text DEFAULT 'durable_route'::text NOT NULL,
    route_id text,
    preview_id text,
    preview_generation bigint,
    preview_state text,
    preview_expires_at timestamp with time zone,
    user_account_id text,
    CONSTRAINT tunnel_certificate_records_certificate_ciphertext_check CHECK (((octet_length(certificate_ciphertext) >= 29) AND (octet_length(certificate_ciphertext) <= 16777216))),
    CONSTRAINT tunnel_certificate_records_certificate_reference_check CHECK (((length(TRIM(BOTH FROM certificate_reference)) >= 1) AND (length(TRIM(BOTH FROM certificate_reference)) <= 256))),
    CONSTRAINT tunnel_certificate_records_check CHECK (((domain_generation > 0) AND (certificate_generation > 0))),
    CONSTRAINT tunnel_certificate_records_check1 CHECK (((expires_at > not_before) AND (renewal_at < expires_at))),
    CONSTRAINT tunnel_certificate_records_check2 CHECK (((state = 'revoked'::text) = (revoked_at IS NOT NULL))),
    CONSTRAINT tunnel_certificate_records_fingerprint_check CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT tunnel_certificate_records_hostname_check CHECK ((((length(TRIM(BOTH FROM hostname)) >= 1) AND (length(TRIM(BOTH FROM hostname)) <= 253)) AND (hostname = lower(hostname)) AND (hostname !~ '[.]$'::text))),
    CONSTRAINT tunnel_certificate_records_id_check CHECK (((length(TRIM(BOTH FROM id)) >= 3) AND (length(TRIM(BOTH FROM id)) <= 128))),
    CONSTRAINT tunnel_certificate_records_issuer_check CHECK (((length(TRIM(BOTH FROM issuer)) >= 1) AND (length(TRIM(BOTH FROM issuer)) <= 256))),
    CONSTRAINT tunnel_certificate_records_leaf_hostname_check CHECK (((leaf_hostname IS NULL) OR ((strategy = 'on_demand_leaf'::text) AND (leaf_hostname = hostname) AND (leaf_hostname = lower(leaf_hostname)) AND (leaf_hostname !~ '[*]'::text) AND (leaf_hostname !~ '[.]$'::text)))),
    CONSTRAINT tunnel_certificate_records_master_key_reference_check CHECK ((((length(TRIM(BOTH FROM master_key_reference)) >= 1) AND (length(TRIM(BOTH FROM master_key_reference)) <= 256)) AND (master_key_reference !~ '[\r\n]'::text))),
    CONSTRAINT tunnel_certificate_records_private_key_ciphertext_check CHECK (((octet_length(private_key_ciphertext) >= 29) AND (octet_length(private_key_ciphertext) <= 16777216))),
    CONSTRAINT tunnel_certificate_records_state_check CHECK ((state = ANY (ARRAY['staged'::text, 'active'::text, 'superseded'::text, 'revoked'::text, 'expired'::text, 'failed'::text]))),
    CONSTRAINT tunnel_certificate_records_strategy_check CHECK ((strategy = ANY (ARRAY['delegated_dns01'::text, 'provided_reference'::text, 'on_demand_leaf'::text, 'wildcard_fallback'::text, 'platform_dns01'::text]))),
    CONSTRAINT tunnel_certificate_records_target_kind_check CHECK ((target_kind = ANY (ARRAY['durable_route'::text, 'preview_lease'::text, 'platform_wildcard'::text]))),
    CONSTRAINT tunnel_certificate_records_target_strategy_check CHECK ((((target_kind = 'platform_wildcard'::text) AND (strategy = 'platform_dns01'::text)) OR ((target_kind <> 'platform_wildcard'::text) AND (strategy <> 'platform_dns01'::text)))),
    CONSTRAINT tunnel_certificate_records_target_xor_check CHECK (((domain_id IS NOT NULL) AND (((target_kind = 'durable_route'::text) AND (tunnel_id IS NOT NULL) AND (preview_id IS NULL) AND (preview_generation IS NULL) AND (preview_state IS NULL) AND (preview_expires_at IS NULL)) OR ((target_kind = 'preview_lease'::text) AND (tunnel_id IS NULL) AND (preview_id IS NOT NULL) AND (preview_generation > 0) AND (preview_state = 'active'::text) AND (preview_expires_at IS NOT NULL)) OR ((target_kind = 'platform_wildcard'::text) AND (tunnel_id IS NULL) AND (route_id IS NULL) AND (preview_id IS NULL) AND (preview_generation IS NULL) AND (preview_state IS NULL) AND (preview_expires_at IS NULL) AND (leaf_hostname IS NULL))))),
    CONSTRAINT tunnel_certificate_records_user_account_match_check CHECK ((((target_kind = 'platform_wildcard'::text) AND (user_account_id IS NULL) AND (account_id = 'platform_system_v1'::text)) OR ((target_kind <> 'platform_wildcard'::text) AND (user_account_id IS NOT NULL) AND (user_account_id = account_id))))
);


--
-- Name: tunnel_config_generations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_config_generations (
    tunnel_id text NOT NULL,
    generation bigint NOT NULL,
    previous_generation bigint,
    content_hash bytea NOT NULL,
    snapshot bytea NOT NULL,
    snapshot_reference text,
    activation_state text DEFAULT 'pending'::text NOT NULL,
    created_by_actor_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    activated_at timestamp with time zone,
    retained_until timestamp with time zone NOT NULL,
    CONSTRAINT tunnel_config_generations_activation_state_check CHECK ((activation_state = ANY (ARRAY['pending'::text, 'active'::text, 'superseded'::text, 'rejected'::text]))),
    CONSTRAINT tunnel_config_generations_check CHECK ((((generation = 1) AND (previous_generation IS NULL)) OR ((generation > 1) AND (previous_generation = (generation - 1))))),
    CONSTRAINT tunnel_config_generations_check1 CHECK (((activation_state = 'active'::text) = (activated_at IS NOT NULL))),
    CONSTRAINT tunnel_config_generations_check2 CHECK ((retained_until > created_at)),
    CONSTRAINT tunnel_config_generations_content_hash_check CHECK ((octet_length(content_hash) = 32)),
    CONSTRAINT tunnel_config_generations_generation_check CHECK ((generation > 0)),
    CONSTRAINT tunnel_config_generations_snapshot_bytes_check CHECK ((octet_length(snapshot) > 0))
);


--
-- Name: tunnel_connector_activations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_connector_activations (
    operation_id text NOT NULL,
    account_id text NOT NULL,
    tunnel_id text NOT NULL,
    connector_id text NOT NULL,
    host_id text NOT NULL,
    credential_generation bigint NOT NULL,
    process_generation bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tunnel_connector_activations_credential_generation_check CHECK ((credential_generation > 0)),
    CONSTRAINT tunnel_connector_activations_process_generation_check CHECK ((process_generation > 0))
);


--
-- Name: tunnel_connector_credential_generations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_connector_credential_generations (
    id text NOT NULL,
    connector_id text NOT NULL,
    tunnel_id text NOT NULL,
    generation bigint NOT NULL,
    credential_reference text NOT NULL,
    credential_thumbprint text NOT NULL,
    verifier_algorithm text DEFAULT 'ed25519'::text NOT NULL,
    verifier_public_key bytea NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    valid_until timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    source_operation_id text,
    CONSTRAINT tunnel_connector_credential_generat_credential_thumbprint_check CHECK ((length(TRIM(BOTH FROM credential_thumbprint)) > 0)),
    CONSTRAINT tunnel_connector_credential_generati_credential_reference_check CHECK ((length(TRIM(BOTH FROM credential_reference)) > 0)),
    CONSTRAINT tunnel_connector_credential_generatio_verifier_public_key_check CHECK ((octet_length(verifier_public_key) = 32)),
    CONSTRAINT tunnel_connector_credential_generation_verifier_algorithm_check CHECK ((verifier_algorithm = 'ed25519'::text)),
    CONSTRAINT tunnel_connector_credential_generations_check CHECK ((valid_until > created_at)),
    CONSTRAINT tunnel_connector_credential_generations_check1 CHECK (((state = 'revoked'::text) = (revoked_at IS NOT NULL))),
    CONSTRAINT tunnel_connector_credential_generations_generation_check CHECK ((generation > 0)),
    CONSTRAINT tunnel_connector_credential_generations_state_check CHECK ((state = ANY (ARRAY['active'::text, 'overlap'::text, 'revoked'::text])))
);


--
-- Name: tunnel_connector_enrollments; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_connector_enrollments (
    id text NOT NULL,
    account_id text NOT NULL,
    tunnel_id text NOT NULL,
    host_id text NOT NULL,
    operation_id text NOT NULL,
    token_hash bytea NOT NULL,
    capabilities text[] DEFAULT '{}'::text[] NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    connector_id text,
    created_by_actor_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tunnel_connector_enrollments_check CHECK ((expires_at > created_at)),
    CONSTRAINT tunnel_connector_enrollments_check1 CHECK (((consumed_at IS NULL) OR (consumed_at >= created_at))),
    CONSTRAINT tunnel_connector_enrollments_consumed_connector_check CHECK (((connector_id IS NULL) OR (consumed_at IS NOT NULL))),
    CONSTRAINT tunnel_connector_enrollments_token_hash_check CHECK ((octet_length(token_hash) = 32))
);


--
-- Name: tunnel_connector_rotation_targets; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_connector_rotation_targets (
    operation_id text NOT NULL,
    account_id text NOT NULL,
    tunnel_id text NOT NULL,
    connector_id text NOT NULL,
    host_id text NOT NULL,
    target_set_hash text NOT NULL,
    old_credential_generation bigint NOT NULL,
    new_credential_generation bigint NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    old_identity_key_id text,
    old_identity_key_thumbprint text,
    challenge_nonce text,
    challenge_issued_at timestamp with time zone,
    challenge_expires_at timestamp with time zone,
    overlap_until timestamp with time zone,
    new_credential_valid_until timestamp with time zone,
    proof_session_id text,
    proof_process_generation bigint,
    new_identity_key_id text,
    new_identity_key_thumbprint text,
    new_public_key bytea,
    new_credential_reference text,
    replacement_session_id text,
    replacement_process_generation bigint,
    config_generation bigint,
    config_content_hash bytea,
    edge_ready boolean,
    route_ready boolean,
    origin_ready boolean,
    ready_at timestamp with time zone,
    revoke_nonce text,
    revoke_session_id text,
    revoke_process_generation bigint,
    revoke_issued_at timestamp with time zone,
    revoke_deadline timestamp with time zone,
    revoked_at timestamp with time zone,
    failure_code text,
    failure_message text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tunnel_connector_rotation_ta_replacement_process_generati_check CHECK (((replacement_process_generation IS NULL) OR (replacement_process_generation > 0))),
    CONSTRAINT tunnel_connector_rotation_targe_old_credential_generation_check CHECK ((old_credential_generation > 0)),
    CONSTRAINT tunnel_connector_rotation_targe_revoke_process_generation_check CHECK (((revoke_process_generation IS NULL) OR (revoke_process_generation > 0))),
    CONSTRAINT tunnel_connector_rotation_target_proof_process_generation_check CHECK (((proof_process_generation IS NULL) OR (proof_process_generation > 0))),
    CONSTRAINT tunnel_connector_rotation_targets_challenge_nonce_check CHECK (((challenge_nonce IS NULL) OR ((length(TRIM(BOTH FROM challenge_nonce)) >= 16) AND (length(TRIM(BOTH FROM challenge_nonce)) <= 128)))),
    CONSTRAINT tunnel_connector_rotation_targets_challenge_nonce_check1 CHECK (((challenge_nonce IS NULL) OR (challenge_nonce !~ '[\r\n]'::text))),
    CONSTRAINT tunnel_connector_rotation_targets_check CHECK ((new_credential_generation = (old_credential_generation + 1))),
    CONSTRAINT tunnel_connector_rotation_targets_check1 CHECK (((state <> ALL (ARRAY['installed'::text, 'ready'::text, 'revoking'::text, 'revoked'::text])) OR ((new_public_key IS NOT NULL) AND (new_credential_reference IS NOT NULL) AND (new_credential_valid_until IS NOT NULL)))),
    CONSTRAINT tunnel_connector_rotation_targets_check2 CHECK (((state <> ALL (ARRAY['ready'::text, 'revoking'::text, 'revoked'::text])) OR ((replacement_session_id IS NOT NULL) AND (replacement_process_generation IS NOT NULL) AND (config_generation IS NOT NULL) AND (config_content_hash IS NOT NULL) AND (edge_ready IS TRUE) AND (route_ready IS TRUE) AND (origin_ready IS TRUE) AND (ready_at IS NOT NULL)))),
    CONSTRAINT tunnel_connector_rotation_targets_check3 CHECK (((state <> ALL (ARRAY['revoking'::text, 'revoked'::text])) OR ((revoke_nonce IS NOT NULL) AND (revoke_session_id IS NOT NULL) AND (revoke_process_generation IS NOT NULL) AND (revoke_deadline IS NOT NULL)))),
    CONSTRAINT tunnel_connector_rotation_targets_check4 CHECK (((state = 'revoked'::text) = (revoked_at IS NOT NULL))),
    CONSTRAINT tunnel_connector_rotation_targets_check5 CHECK (((state = 'failed'::text) = (failure_code IS NOT NULL))),
    CONSTRAINT tunnel_connector_rotation_targets_check6 CHECK (((new_credential_valid_until IS NULL) OR (overlap_until IS NULL) OR (new_credential_valid_until > overlap_until))),
    CONSTRAINT tunnel_connector_rotation_targets_config_content_hash_check CHECK (((config_content_hash IS NULL) OR (octet_length(config_content_hash) = 32))),
    CONSTRAINT tunnel_connector_rotation_targets_config_generation_check CHECK (((config_generation IS NULL) OR (config_generation > 0))),
    CONSTRAINT tunnel_connector_rotation_targets_new_public_key_check CHECK (((new_public_key IS NULL) OR (octet_length(new_public_key) = 32))),
    CONSTRAINT tunnel_connector_rotation_targets_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'challenged'::text, 'installed'::text, 'ready'::text, 'revoking'::text, 'revoked'::text, 'failed'::text]))),
    CONSTRAINT tunnel_connector_rotation_targets_target_set_hash_check CHECK ((target_set_hash ~ '^sha256:[0-9a-f]{64}$'::text))
);


--
-- Name: tunnel_connector_sessions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_connector_sessions (
    id text NOT NULL,
    connector_id text NOT NULL,
    process_generation bigint NOT NULL,
    protocol_version text NOT NULL,
    capabilities text[] DEFAULT '{}'::text[] NOT NULL,
    state text DEFAULT 'authenticating'::text NOT NULL,
    lease_deadline timestamp with time zone NOT NULL,
    last_heartbeat_at timestamp with time zone DEFAULT now() NOT NULL,
    ready_at timestamp with time zone,
    disconnected_at timestamp with time zone,
    disconnect_reason_code text,
    applied_config_generation bigint DEFAULT 0 NOT NULL,
    retained_until timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    credential_generation bigint DEFAULT 1 NOT NULL,
    last_heartbeat_sent_at timestamp with time zone,
    CONSTRAINT tunnel_connector_sessions_applied_config_generation_check CHECK ((applied_config_generation >= 0)),
    CONSTRAINT tunnel_connector_sessions_check CHECK ((lease_deadline > created_at)),
    CONSTRAINT tunnel_connector_sessions_check1 CHECK ((retained_until >= lease_deadline)),
    CONSTRAINT tunnel_connector_sessions_check2 CHECK (((state = ANY (ARRAY['disconnected'::text, 'expired'::text])) = (disconnected_at IS NOT NULL))),
    CONSTRAINT tunnel_connector_sessions_credential_generation_check CHECK ((credential_generation > 0)),
    CONSTRAINT tunnel_connector_sessions_process_generation_check CHECK ((process_generation > 0)),
    CONSTRAINT tunnel_connector_sessions_state_check CHECK ((state = ANY (ARRAY['authenticating'::text, 'ready'::text, 'draining'::text, 'disconnected'::text, 'expired'::text])))
);


--
-- Name: tunnel_connectors; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_connectors (
    id text NOT NULL,
    tunnel_id text NOT NULL,
    host_id text NOT NULL,
    credential_reference text NOT NULL,
    credential_thumbprint text NOT NULL,
    rotation_generation bigint DEFAULT 1 NOT NULL,
    desired_state text DEFAULT 'active'::text NOT NULL,
    software_version text,
    protocol_version text NOT NULL,
    operating_system text,
    architecture text,
    last_session_id text,
    last_heartbeat_at timestamp with time zone,
    ready_at timestamp with time zone,
    disconnect_reason_code text,
    last_applied_config_generation bigint DEFAULT 0 NOT NULL,
    drain_state text DEFAULT 'accepting'::text NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    CONSTRAINT tunnel_connectors_check CHECK (((desired_state = 'revoked'::text) = (revoked_at IS NOT NULL))),
    CONSTRAINT tunnel_connectors_credential_reference_check CHECK ((length(TRIM(BOTH FROM credential_reference)) > 0)),
    CONSTRAINT tunnel_connectors_credential_thumbprint_check CHECK ((length(TRIM(BOTH FROM credential_thumbprint)) > 0)),
    CONSTRAINT tunnel_connectors_desired_state_check CHECK ((desired_state = ANY (ARRAY['active'::text, 'draining'::text, 'revoked'::text]))),
    CONSTRAINT tunnel_connectors_drain_state_check CHECK ((drain_state = ANY (ARRAY['accepting'::text, 'draining'::text, 'drained'::text, 'forced_closed'::text]))),
    CONSTRAINT tunnel_connectors_generation_check CHECK ((generation > 0)),
    CONSTRAINT tunnel_connectors_last_applied_config_generation_check CHECK ((last_applied_config_generation >= 0)),
    CONSTRAINT tunnel_connectors_rotation_generation_check CHECK ((rotation_generation > 0))
);


--
-- Name: tunnel_edge_route_assignments; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_edge_route_assignments (
    assignment_id text NOT NULL,
    route_id text NOT NULL,
    assignment_generation bigint NOT NULL,
    account_id text NOT NULL,
    tunnel_id text NOT NULL,
    connector_id text NOT NULL,
    host_id text NOT NULL,
    machine_identity_public_key text NOT NULL,
    machine_identity_thumbprint text NOT NULL,
    connector_generation bigint NOT NULL,
    connector_session_id text NOT NULL,
    connector_process_generation bigint NOT NULL,
    config_generation bigint NOT NULL,
    config_content_hash bytea NOT NULL,
    access_mode text DEFAULT 'public'::text NOT NULL,
    route_generation bigint NOT NULL,
    route_revision bigint NOT NULL,
    edge_node_id text NOT NULL,
    edge_process_epoch text NOT NULL,
    edge_failure_domain text NOT NULL,
    state text DEFAULT 'staged'::text NOT NULL,
    observed_state text DEFAULT 'pending'::text NOT NULL,
    assigned_at timestamp with time zone DEFAULT now() NOT NULL,
    observed_at timestamp with time zone,
    released_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tunnel_edge_route_assignment_connector_process_generation_check CHECK ((connector_process_generation > 0)),
    CONSTRAINT tunnel_edge_route_assignments_access_mode_check CHECK ((access_mode = ANY (ARRAY['public'::text, 'private'::text, 'team'::text]))),
    CONSTRAINT tunnel_edge_route_assignments_assignment_generation_check CHECK ((assignment_generation > 0)),
    CONSTRAINT tunnel_edge_route_assignments_assignment_id_check CHECK (((length(TRIM(BOTH FROM assignment_id)) >= 3) AND (length(TRIM(BOTH FROM assignment_id)) <= 128))),
    CONSTRAINT tunnel_edge_route_assignments_check CHECK (((route_generation > 0) AND (route_revision > 0))),
    CONSTRAINT tunnel_edge_route_assignments_check1 CHECK ((route_revision = route_generation)),
    CONSTRAINT tunnel_edge_route_assignments_check2 CHECK (((observed_state <> 'ready'::text) OR (state = ANY (ARRAY['staged'::text, 'active'::text])))),
    CONSTRAINT tunnel_edge_route_assignments_check3 CHECK (((state = 'detached'::text) = (released_at IS NOT NULL))),
    CONSTRAINT tunnel_edge_route_assignments_config_content_hash_check CHECK ((octet_length(config_content_hash) = 32)),
    CONSTRAINT tunnel_edge_route_assignments_config_generation_check CHECK ((config_generation > 0)),
    CONSTRAINT tunnel_edge_route_assignments_connector_generation_check CHECK ((connector_generation > 0)),
    CONSTRAINT tunnel_edge_route_assignments_edge_failure_domain_check CHECK (((length(TRIM(BOTH FROM edge_failure_domain)) >= 1) AND (length(TRIM(BOTH FROM edge_failure_domain)) <= 128))),
    CONSTRAINT tunnel_edge_route_assignments_edge_process_epoch_check CHECK ((((length(edge_process_epoch) >= 8) AND (length(edge_process_epoch) <= 128)) AND (edge_process_epoch ~ '^[A-Za-z0-9_-]+$'::text))),
    CONSTRAINT tunnel_edge_route_assignments_machine_identity_public_key_check CHECK ((machine_identity_public_key ~ '^[A-Za-z0-9_-]{43}$'::text)),
    CONSTRAINT tunnel_edge_route_assignments_machine_identity_thumbprint_check CHECK ((machine_identity_thumbprint ~ '^[A-Za-z0-9_-]{43}$'::text)),
    CONSTRAINT tunnel_edge_route_assignments_observed_state_check CHECK ((observed_state = ANY (ARRAY['pending'::text, 'ready'::text, 'degraded'::text, 'draining'::text, 'detached'::text, 'failed'::text]))),
    CONSTRAINT tunnel_edge_route_assignments_state_check CHECK ((state = ANY (ARRAY['staged'::text, 'active'::text, 'draining'::text, 'detached'::text])))
);


--
-- Name: tunnel_log_entries; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_log_entries (
    id text NOT NULL,
    account_id text NOT NULL,
    tunnel_id text,
    preview_id text,
    route_id text,
    connector_id text,
    session_id text,
    level text NOT NULL,
    component text NOT NULL,
    code text NOT NULL,
    message text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    correlation_id text NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    cursor_sequence bigint NOT NULL,
    CONSTRAINT tunnel_log_entries_check CHECK (((tunnel_id IS NULL) <> (preview_id IS NULL))),
    CONSTRAINT tunnel_log_entries_check1 CHECK (((preview_id IS NULL) OR ((route_id IS NULL) AND (connector_id IS NULL)))),
    CONSTRAINT tunnel_log_entries_code_check CHECK ((code ~ '^[a-z][a-z0-9_.-]{0,79}$'::text)),
    CONSTRAINT tunnel_log_entries_component_check CHECK (((length(TRIM(BOTH FROM component)) >= 1) AND (length(TRIM(BOTH FROM component)) <= 40))),
    CONSTRAINT tunnel_log_entries_correlation_id_check CHECK (((length(TRIM(BOTH FROM correlation_id)) >= 1) AND (length(TRIM(BOTH FROM correlation_id)) <= 256))),
    CONSTRAINT tunnel_log_entries_level_check CHECK ((level = ANY (ARRAY['debug'::text, 'info'::text, 'warn'::text, 'error'::text]))),
    CONSTRAINT tunnel_log_entries_message_check CHECK (((length(TRIM(BOTH FROM message)) >= 1) AND (length(TRIM(BOTH FROM message)) <= 1000))),
    CONSTRAINT tunnel_log_entries_metadata_check CHECK ((octet_length((metadata)::text) <= 32768)),
    CONSTRAINT tunnel_log_entries_metadata_check1 CHECK ((jsonb_typeof(metadata) = 'object'::text))
);


--
-- Name: tunnel_log_entries_cursor_sequence_seq; Type: SEQUENCE; Schema: paperboat; Owner: -
--

ALTER TABLE paperboat.tunnel_log_entries ALTER COLUMN cursor_sequence ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME paperboat.tunnel_log_entries_cursor_sequence_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: tunnel_platform_certificate_targets; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.tunnel_platform_certificate_targets (
    id text NOT NULL,
    kind text NOT NULL,
    hostname text NOT NULL,
    account_id text NOT NULL,
    challenge_reference text NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    desired_state text DEFAULT 'active'::text NOT NULL,
    certificate_state text DEFAULT 'pending'::text NOT NULL,
    certificate_reference text,
    certificate_expires_at timestamp with time zone,
    certificate_renewal_attempted_at timestamp with time zone,
    certificate_failure_code text,
    retry_count integer DEFAULT 0 NOT NULL,
    next_retry_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tunnel_platform_certificate_targ_certificate_failure_code_check CHECK (((certificate_failure_code IS NULL) OR ((length(TRIM(BOTH FROM certificate_failure_code)) >= 1) AND (length(TRIM(BOTH FROM certificate_failure_code)) <= 128)))),
    CONSTRAINT tunnel_platform_certificate_targets_account_id_check CHECK ((account_id = 'platform_system_v1'::text)),
    CONSTRAINT tunnel_platform_certificate_targets_certificate_reference_check CHECK (((certificate_reference IS NULL) OR ((length(TRIM(BOTH FROM certificate_reference)) >= 1) AND (length(TRIM(BOTH FROM certificate_reference)) <= 256)))),
    CONSTRAINT tunnel_platform_certificate_targets_certificate_state_check CHECK ((certificate_state = ANY (ARRAY['pending'::text, 'failed'::text, 'ready'::text, 'revoked'::text]))),
    CONSTRAINT tunnel_platform_certificate_targets_challenge_reference_check CHECK ((((length(TRIM(BOTH FROM challenge_reference)) >= 1) AND (length(TRIM(BOTH FROM challenge_reference)) <= 256)) AND (challenge_reference !~ '[\r\n]'::text))),
    CONSTRAINT tunnel_platform_certificate_targets_check CHECK ((((id = 'platform_cert_preview_v1'::text) AND (kind = 'preview_wildcard'::text)) OR ((id = 'platform_cert_tunnel_v1'::text) AND (kind = 'tunnel_wildcard'::text)) OR ((id = 'platform_cert_runtime_v1'::text) AND (kind = 'runtime_wildcard'::text)))),
    CONSTRAINT tunnel_platform_certificate_targets_check1 CHECK (((certificate_expires_at IS NULL) OR (certificate_expires_at > created_at))),
    CONSTRAINT tunnel_platform_certificate_targets_check2 CHECK (((next_retry_at IS NULL) OR (next_retry_at > updated_at))),
    CONSTRAINT tunnel_platform_certificate_targets_check3 CHECK (((certificate_state = 'revoked'::text) = (desired_state = 'revoked'::text))),
    CONSTRAINT tunnel_platform_certificate_targets_desired_state_check CHECK ((desired_state = ANY (ARRAY['active'::text, 'revoked'::text]))),
    CONSTRAINT tunnel_platform_certificate_targets_generation_check CHECK ((generation > 0)),
    CONSTRAINT tunnel_platform_certificate_targets_hostname_check CHECK ((((length(TRIM(BOTH FROM hostname)) >= 3) AND (length(TRIM(BOTH FROM hostname)) <= 253)) AND (hostname = lower(hostname)) AND (hostname ~~ '*.%'::text) AND (hostname !~ '[.]$'::text))),
    CONSTRAINT tunnel_platform_certificate_targets_id_check CHECK ((id = ANY (ARRAY['platform_cert_preview_v1'::text, 'platform_cert_tunnel_v1'::text, 'platform_cert_runtime_v1'::text]))),
    CONSTRAINT tunnel_platform_certificate_targets_kind_check CHECK ((kind = ANY (ARRAY['preview_wildcard'::text, 'tunnel_wildcard'::text, 'runtime_wildcard'::text]))),
    CONSTRAINT tunnel_platform_certificate_targets_retry_count_check CHECK (((retry_count >= 0) AND (retry_count <= 30)))
);


--
-- Name: usage_accounting_authorities; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.usage_accounting_authorities (
    authority_id text NOT NULL,
    edge_node_id text NOT NULL,
    environment_id text NOT NULL,
    route_id text NOT NULL,
    route_revision bigint NOT NULL,
    workspace_id text NOT NULL,
    admitted_at timestamp with time zone NOT NULL,
    admission_expires_at timestamp with time zone NOT NULL,
    retain_until timestamp with time zone NOT NULL,
    CONSTRAINT usage_accounting_authorities_check CHECK (((admission_expires_at >= admitted_at) AND (retain_until > admission_expires_at))),
    CONSTRAINT usage_accounting_authorities_route_revision_check CHECK ((route_revision > 0))
);


--
-- Name: user_favorites; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_favorites (
    user_id text NOT NULL,
    kind text NOT NULL,
    resource_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_favorites_kind_check CHECK ((kind = ANY (ARRAY['machine'::text, 'session'::text, 'preview'::text])))
);


--
-- Name: user_identities; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_identities (
    id text NOT NULL,
    user_id text NOT NULL,
    provider text NOT NULL,
    provider_subject text NOT NULL,
    email text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: user_machine_access_sessions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_machine_access_sessions (
    id text NOT NULL,
    user_machine_id text NOT NULL,
    user_id text NOT NULL,
    environment_id text NOT NULL,
    cli_client_session_id text NOT NULL,
    http_base_url text NOT NULL,
    helper_terminal_session_id text,
    helper_file_session_id text,
    state text DEFAULT 'active'::text NOT NULL,
    revocation_reason text,
    revoked_at timestamp with time zone,
    helper_revoked_at timestamp with time zone,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    team_id text,
    capabilities text[] DEFAULT '{}'::text[] NOT NULL,
    operation_id text DEFAULT ''::text NOT NULL,
    terminal_session_id text,
    terminal_role text,
    terminal_grant_generation bigint,
    terminal_binding_generation bigint,
    terminal_membership_generation bigint,
    terminal_team_generation bigint,
    device_service_port integer,
    device_service_source_kind text,
    device_service_source_id text,
    device_service_source_generation bigint,
    device_service_team_fence text,
    CONSTRAINT machine_access_capabilities_valid CHECK (((cardinality(capabilities) <= 7) AND (capabilities <@ ARRAY['terminal'::text, 'exec'::text, 'managed_ssh'::text, 'files'::text, 'preview_manage'::text, 'tunnel_manage'::text, 'private_access'::text]))),
    CONSTRAINT machine_access_session_purpose CHECK (((helper_terminal_session_id IS NOT NULL) OR (helper_file_session_id IS NOT NULL) OR ((device_service_port IS NOT NULL) AND (capabilities = ARRAY['private_access'::text]) AND (device_service_source_kind = ANY (ARRAY['automatic'::text, 'lazy_policy'::text, 'tunnel'::text])) AND (device_service_source_id IS NOT NULL) AND (device_service_source_generation > 0)))),
    CONSTRAINT user_machine_access_sessions_device_service_port_check CHECK (((device_service_port >= 1) AND (device_service_port <= 65535))),
    CONSTRAINT user_machine_access_sessions_state_check CHECK ((state = ANY (ARRAY['active'::text, 'revoked'::text]))),
    CONSTRAINT user_machine_access_sessions_terminal_role_check CHECK ((terminal_role = ANY (ARRAY['owner'::text, 'viewer'::text, 'interactive'::text])))
);


--
-- Name: user_machine_availability_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_machine_availability_operations (
    id text NOT NULL,
    user_machine_id text NOT NULL,
    user_id text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash bytea NOT NULL,
    expected_version bigint NOT NULL,
    resulting_version bigint NOT NULL,
    mode text NOT NULL,
    result jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_machine_availability_operations_expected_version_check CHECK ((expected_version >= 0)),
    CONSTRAINT user_machine_availability_operations_mode_check CHECK ((mode = ANY (ARRAY['allow_sleep'::text, 'keep_awake'::text]))),
    CONSTRAINT user_machine_availability_operations_request_hash_check CHECK ((octet_length(request_hash) = 32)),
    CONSTRAINT user_machine_availability_operations_resulting_version_check CHECK ((resulting_version > 0))
);


--
-- Name: user_machine_bandwidth_periods; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_machine_bandwidth_periods (
    id text NOT NULL,
    user_machine_id text NOT NULL,
    period_start timestamp with time zone NOT NULL,
    period_end timestamp with time zone NOT NULL,
    included_bytes bigint NOT NULL,
    consumed_included_bytes bigint DEFAULT 0 NOT NULL,
    consumed_topup_bytes bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_machine_bandwidth_periods_check CHECK ((period_end > period_start)),
    CONSTRAINT user_machine_bandwidth_periods_consumed_included_bytes_check CHECK ((consumed_included_bytes >= 0)),
    CONSTRAINT user_machine_bandwidth_periods_consumed_topup_bytes_check CHECK ((consumed_topup_bytes >= 0)),
    CONSTRAINT user_machine_bandwidth_periods_included_bytes_check CHECK ((included_bytes >= 0))
);


--
-- Name: user_machine_bandwidth_topups; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_machine_bandwidth_topups (
    id text NOT NULL,
    user_id text NOT NULL,
    provider_order_id text,
    purchased_bytes bigint NOT NULL,
    remaining_bytes bigint NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    expires_at timestamp with time zone,
    consumed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_machine_bandwidth_topups_check CHECK (((remaining_bytes >= 0) AND (remaining_bytes <= purchased_bytes))),
    CONSTRAINT user_machine_bandwidth_topups_purchased_bytes_check CHECK ((purchased_bytes > 0)),
    CONSTRAINT user_machine_bandwidth_topups_state_check CHECK ((state = ANY (ARRAY['active'::text, 'exhausted'::text, 'void'::text])))
);


--
-- Name: user_machine_capability_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_machine_capability_operations (
    id text NOT NULL,
    user_machine_id text NOT NULL,
    user_id text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash bytea NOT NULL,
    expected_version bigint NOT NULL,
    resulting_version bigint NOT NULL,
    configured_capabilities text[] NOT NULL,
    result jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_machine_capability_operations_check CHECK ((resulting_version = (expected_version + 1))),
    CONSTRAINT user_machine_capability_operations_expected_version_check CHECK ((expected_version > 0)),
    CONSTRAINT user_machine_capability_operations_request_hash_check CHECK ((octet_length(request_hash) = 32))
);


--
-- Name: user_machine_enrollments; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_machine_enrollments (
    id text NOT NULL,
    user_id text NOT NULL,
    operation_id text NOT NULL,
    idempotency_key text NOT NULL,
    bootstrap_token_hash bytea NOT NULL,
    bootstrap_token_ciphertext bytea NOT NULL,
    state text DEFAULT 'awaiting_bootstrap'::text NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    pairing_id text,
    user_machine_id text,
    requested_alias text,
    platform text,
    architecture text,
    workspace_root text,
    expires_at timestamp with time zone NOT NULL,
    cancelled_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    requested_description text DEFAULT ''::text NOT NULL,
    CONSTRAINT user_machine_enrollments_generation_check CHECK ((generation > 0)),
    CONSTRAINT user_machine_enrollments_requested_description_check CHECK (((char_length(requested_description) <= 160) AND (requested_description !~ '[[:cntrl:]]'::text))),
    CONSTRAINT user_machine_enrollments_state_check CHECK ((state = ANY (ARRAY['awaiting_bootstrap'::text, 'awaiting_approval'::text, 'approved'::text, 'material_issued'::text, 'installing'::text, 'connecting'::text, 'ready'::text, 'cancelled'::text, 'expired'::text, 'denied'::text, 'failed_retryable'::text, 'revoked'::text, 'disconnected'::text, 'deleted'::text, 'auth_consumed'::text]))),
    CONSTRAINT user_machine_enrollments_workspace_root_check CHECK (((workspace_root IS NULL) OR (workspace_root ~ '^/'::text) OR ((workspace_root ~ '^[A-Za-z]:'::text) AND (SUBSTRING(workspace_root FROM 3 FOR 1) = chr(92))) OR (SUBSTRING(workspace_root FROM 1 FOR 2) = (chr(92) || chr(92)))))
);


--
-- Name: user_machine_entitlements; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_machine_entitlements (
    id text NOT NULL,
    user_id text NOT NULL,
    provider_subscription_id text NOT NULL,
    product_code text NOT NULL,
    state text NOT NULL,
    seat_quantity integer NOT NULL,
    allowance_bytes bigint NOT NULL,
    current_period_start timestamp with time zone NOT NULL,
    current_period_end timestamp with time zone NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_machine_entitlements_allowance_bytes_check CHECK ((allowance_bytes >= 0)),
    CONSTRAINT user_machine_entitlements_check CHECK ((current_period_end > current_period_start)),
    CONSTRAINT user_machine_entitlements_seat_quantity_check CHECK ((seat_quantity >= 0)),
    CONSTRAINT user_machine_entitlements_state_check CHECK ((state = ANY (ARRAY['trialing'::text, 'active'::text, 'past_due'::text, 'canceled'::text, 'revoked'::text])))
);


--
-- Name: user_machine_maintenance_approvals; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_machine_maintenance_approvals (
    id text NOT NULL,
    user_machine_id text NOT NULL,
    user_id text NOT NULL,
    schema text NOT NULL,
    action text NOT NULL,
    target_version text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    decided_at timestamp with time zone,
    decided_by_user_id text,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_machine_maintenance_approvals_action_check CHECK ((action = ANY (ARRAY['update'::text, 'restart'::text, 'migration'::text]))),
    CONSTRAINT user_machine_maintenance_approvals_check CHECK ((((status = 'pending'::text) AND (decided_at IS NULL) AND (decided_by_user_id IS NULL)) OR (status <> 'pending'::text))),
    CONSTRAINT user_machine_maintenance_approvals_idempotency_key_check CHECK (((length(TRIM(BOTH FROM idempotency_key)) >= 8) AND (length(TRIM(BOTH FROM idempotency_key)) <= 128))),
    CONSTRAINT user_machine_maintenance_approvals_reason_check CHECK ((length(reason) <= 512)),
    CONSTRAINT user_machine_maintenance_approvals_schema_check CHECK (((length(TRIM(BOTH FROM schema)) >= 1) AND (length(TRIM(BOTH FROM schema)) <= 128))),
    CONSTRAINT user_machine_maintenance_approvals_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'approved'::text, 'rejected'::text, 'expired'::text, 'consumed'::text]))),
    CONSTRAINT user_machine_maintenance_approvals_target_version_check CHECK (((length(TRIM(BOTH FROM target_version)) >= 1) AND (length(TRIM(BOTH FROM target_version)) <= 64))),
    CONSTRAINT user_machine_maintenance_approvals_version_check CHECK ((version > 0))
);


--
-- Name: user_machine_pairings; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_machine_pairings (
    id text NOT NULL,
    verifier_hash bytea NOT NULL,
    user_code text NOT NULL,
    requested_alias text NOT NULL,
    platform text NOT NULL,
    architecture text NOT NULL,
    workspace_root text NOT NULL,
    runtime_versions jsonb DEFAULT '{}'::jsonb NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    approved_by_user_id text,
    user_machine_id text,
    installation_config_ciphertext bytea,
    installation_config_nonce bytea,
    installation_config_consumed_at timestamp with time zone,
    expires_at timestamp with time zone NOT NULL,
    approved_at timestamp with time zone,
    denied_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    public_identity_key text NOT NULL,
    ssh_user text,
    ssh_port integer,
    can_reuse_runtime_identity boolean DEFAULT false NOT NULL,
    installation_recovery_operation_key text,
    authenticated_setup_cli_session_id text,
    authenticated_setup_operation_id text,
    authenticated_setup_generation bigint,
    authenticated_setup_mode text,
    authenticated_setup_helper_enrollment_id text,
    keep_awake boolean DEFAULT false NOT NULL,
    requested_description text DEFAULT ''::text NOT NULL,
    CONSTRAINT user_machine_pairings_authenticated_setup_check CHECK ((((authenticated_setup_cli_session_id IS NULL) AND (authenticated_setup_operation_id IS NULL) AND (authenticated_setup_generation IS NULL) AND (authenticated_setup_mode IS NULL) AND (authenticated_setup_helper_enrollment_id IS NULL)) OR ((authenticated_setup_cli_session_id IS NOT NULL) AND (authenticated_setup_operation_id IS NOT NULL) AND ((length(authenticated_setup_operation_id) >= 8) AND (length(authenticated_setup_operation_id) <= 128)) AND (authenticated_setup_generation IS NOT NULL) AND (authenticated_setup_generation >= 1) AND (authenticated_setup_mode IS NOT NULL) AND (authenticated_setup_mode = 'host'::text) AND (approved_by_user_id IS NOT NULL) AND (user_machine_id IS NOT NULL) AND (state = ANY (ARRAY['approved'::text, 'consumed'::text, 'expired'::text]))))),
    CONSTRAINT user_machine_pairings_public_identity_key_check CHECK (((length(public_identity_key) >= 40) AND (length(public_identity_key) <= 256))),
    CONSTRAINT user_machine_pairings_requested_description_check CHECK (((char_length(requested_description) <= 160) AND (requested_description !~ '[[:cntrl:]]'::text))),
    CONSTRAINT user_machine_pairings_ssh_target_check CHECK ((((ssh_user IS NULL) AND (ssh_port IS NULL)) OR ((ssh_user IS NOT NULL) AND (ssh_user = btrim(ssh_user)) AND ((length(ssh_user) >= 1) AND (length(ssh_user) <= 128)) AND (ssh_user !~ '[\x00\r\n]'::text) AND ((ssh_port >= 1) AND (ssh_port <= 65535))))),
    CONSTRAINT user_machine_pairings_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'approved'::text, 'denied'::text, 'expired'::text, 'consumed'::text]))),
    CONSTRAINT user_machine_pairings_user_code_check CHECK (((length(TRIM(BOTH FROM user_code)) >= 4) AND (length(TRIM(BOTH FROM user_code)) <= 32))),
    CONSTRAINT user_machine_pairings_workspace_root_check CHECK (((workspace_root ~ '^/'::text) OR ((workspace_root ~ '^[A-Za-z]:'::text) AND (SUBSTRING(workspace_root FROM 3 FOR 1) = chr(92))) OR (SUBSTRING(workspace_root FROM 1 FOR 2) = (chr(92) || chr(92)))))
);


--
-- Name: user_machine_terminal_session_operations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_machine_terminal_session_operations (
    id text NOT NULL,
    user_machine_id text NOT NULL,
    terminal_session_id text NOT NULL,
    operation text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    last_error text,
    completed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_machine_terminal_session_operations_operation_check CHECK ((operation = ANY (ARRAY['close'::text, 'delete_history'::text]))),
    CONSTRAINT user_machine_terminal_session_operations_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'applied'::text, 'failed'::text, 'superseded'::text])))
);


--
-- Name: user_machine_terminal_sessions; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_machine_terminal_sessions (
    id text NOT NULL,
    user_machine_id text NOT NULL,
    terminal_id text NOT NULL,
    thread_id text DEFAULT 'paperboat'::text NOT NULL,
    name text NOT NULL,
    idempotency_key text,
    is_default boolean DEFAULT false NOT NULL,
    auto_name_ordinal integer,
    launch_cwd text NOT NULL,
    desired_state text DEFAULT 'open'::text NOT NULL,
    runtime_state text DEFAULT 'unknown'::text NOT NULL,
    last_activity_at timestamp with time zone,
    last_runtime_sync_at timestamp with time zone,
    last_runtime_sequence bigint,
    deleted_at timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    transfer_destination_machine_id text,
    owner_account text NOT NULL,
    title text,
    CONSTRAINT user_machine_terminal_sessions_check CHECK (((is_default AND (name = 'default'::text)) OR (NOT is_default))),
    CONSTRAINT user_machine_terminal_sessions_desired_state_check CHECK ((desired_state = ANY (ARRAY['open'::text, 'closed'::text, 'deleted'::text]))),
    CONSTRAINT user_machine_terminal_sessions_name_check CHECK ((name ~ '^[a-z0-9][a-z0-9._-]{0,63}$'::text)),
    CONSTRAINT user_machine_terminal_sessions_title_valid CHECK (((title IS NULL) OR (((char_length(title) >= 1) AND (char_length(title) <= 128)) AND (title !~ '[[:cntrl:]]'::text))))
);


--
-- Name: user_machine_update_observations; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_machine_update_observations (
    user_machine_id text NOT NULL,
    environment_id text NOT NULL,
    schema text NOT NULL,
    current_version text NOT NULL,
    target_version text,
    channel text NOT NULL,
    state text NOT NULL,
    error_code text,
    operation_id text NOT NULL,
    installation_generation bigint NOT NULL,
    worker_generation bigint NOT NULL,
    os_boot_id text NOT NULL,
    rollback_count bigint DEFAULT 0 NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    payload_hash bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_machine_update_observations_channel_check CHECK (((length(TRIM(BOTH FROM channel)) >= 1) AND (length(TRIM(BOTH FROM channel)) <= 32))),
    CONSTRAINT user_machine_update_observations_check CHECK ((((state = ANY (ARRAY['failed'::text, 'rolled_back'::text])) AND (error_code IS NOT NULL)) OR ((state <> ALL (ARRAY['failed'::text, 'rolled_back'::text])) AND (error_code IS NULL)))),
    CONSTRAINT user_machine_update_observations_current_version_check CHECK (((length(TRIM(BOTH FROM current_version)) >= 1) AND (length(TRIM(BOTH FROM current_version)) <= 64))),
    CONSTRAINT user_machine_update_observations_installation_generation_check CHECK ((installation_generation > 0)),
    CONSTRAINT user_machine_update_observations_operation_id_check CHECK (((length(TRIM(BOTH FROM operation_id)) >= 8) AND (length(TRIM(BOTH FROM operation_id)) <= 128))),
    CONSTRAINT user_machine_update_observations_os_boot_id_check CHECK (((length(TRIM(BOTH FROM os_boot_id)) >= 1) AND (length(TRIM(BOTH FROM os_boot_id)) <= 256))),
    CONSTRAINT user_machine_update_observations_rollback_count_check CHECK ((rollback_count >= 0)),
    CONSTRAINT user_machine_update_observations_schema_check CHECK (((length(TRIM(BOTH FROM schema)) >= 1) AND (length(TRIM(BOTH FROM schema)) <= 128))),
    CONSTRAINT user_machine_update_observations_state_check CHECK ((state = ANY (ARRAY['idle'::text, 'checking'::text, 'downloading'::text, 'staged'::text, 'activating'::text, 'deferred'::text, 'healthy'::text, 'failed'::text, 'rolled_back'::text]))),
    CONSTRAINT user_machine_update_observations_target_version_check CHECK (((target_version IS NULL) OR ((length(TRIM(BOTH FROM target_version)) >= 1) AND (length(TRIM(BOTH FROM target_version)) <= 64)))),
    CONSTRAINT user_machine_update_observations_worker_generation_check CHECK ((worker_generation > 0))
);


--
-- Name: user_transfer_destination_defaults; Type: TABLE; Schema: paperboat; Owner: -
--

CREATE TABLE paperboat.user_transfer_destination_defaults (
    user_id text NOT NULL,
    machine_id text NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_transfer_destination_defaults_version_check CHECK ((version > 0))
);


--
-- Name: account_e2ee_keys account_e2ee_keys_fingerprint_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_e2ee_keys
    ADD CONSTRAINT account_e2ee_keys_fingerprint_key UNIQUE (fingerprint);


--
-- Name: account_e2ee_keys account_e2ee_keys_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_e2ee_keys
    ADD CONSTRAINT account_e2ee_keys_pkey PRIMARY KEY (key_id);


--
-- Name: account_e2ee_keys account_e2ee_keys_public_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_e2ee_keys
    ADD CONSTRAINT account_e2ee_keys_public_key_key UNIQUE (public_key);


--
-- Name: account_e2ee_keys account_e2ee_keys_user_id_fingerprint_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_e2ee_keys
    ADD CONSTRAINT account_e2ee_keys_user_id_fingerprint_key UNIQUE (user_id, fingerprint);


--
-- Name: account_e2ee_keys account_e2ee_keys_user_id_key_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_e2ee_keys
    ADD CONSTRAINT account_e2ee_keys_user_id_key_id_key UNIQUE (user_id, key_id);


--
-- Name: account_e2ee_roots account_e2ee_roots_fingerprint_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_e2ee_roots
    ADD CONSTRAINT account_e2ee_roots_fingerprint_key UNIQUE (fingerprint);


--
-- Name: account_e2ee_roots account_e2ee_roots_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_e2ee_roots
    ADD CONSTRAINT account_e2ee_roots_pkey PRIMARY KEY (user_id);


--
-- Name: account_e2ee_roots account_e2ee_roots_public_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_e2ee_roots
    ADD CONSTRAINT account_e2ee_roots_public_key_key UNIQUE (public_key);


--
-- Name: account_network_preferences account_network_preferences_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_network_preferences
    ADD CONSTRAINT account_network_preferences_pkey PRIMARY KEY (account_id);


--
-- Name: audit_events audit_events_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.audit_events
    ADD CONSTRAINT audit_events_pkey PRIMARY KEY (id);


--
-- Name: auth_enrollment_redemptions auth_enrollment_redemptions_cli_client_session_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.auth_enrollment_redemptions
    ADD CONSTRAINT auth_enrollment_redemptions_cli_client_session_id_key UNIQUE (cli_client_session_id);


--
-- Name: auth_enrollment_redemptions auth_enrollment_redemptions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.auth_enrollment_redemptions
    ADD CONSTRAINT auth_enrollment_redemptions_pkey PRIMARY KEY (enrollment_id);


--
-- Name: auth_rate_limits auth_rate_limits_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.auth_rate_limits
    ADD CONSTRAINT auth_rate_limits_pkey PRIMARY KEY (bucket_key, window_start);


--
-- Name: backup_reports backup_reports_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.backup_reports
    ADD CONSTRAINT backup_reports_pkey PRIMARY KEY (database_id, run_id);


--
-- Name: billing_checkout_reservations billing_checkout_reservations_idempotency_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_checkout_reservations
    ADD CONSTRAINT billing_checkout_reservations_idempotency_key_key UNIQUE (idempotency_key);


--
-- Name: billing_checkout_reservations billing_checkout_reservations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_checkout_reservations
    ADD CONSTRAINT billing_checkout_reservations_pkey PRIMARY KEY (id);


--
-- Name: billing_checkout_reservations billing_checkout_reservations_user_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_checkout_reservations
    ADD CONSTRAINT billing_checkout_reservations_user_id_key UNIQUE (user_id);


--
-- Name: billing_operator_operations billing_operator_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_operator_operations
    ADD CONSTRAINT billing_operator_operations_pkey PRIMARY KEY (idempotency_key);


--
-- Name: billing_operator_policy billing_operator_policy_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_operator_policy
    ADD CONSTRAINT billing_operator_policy_pkey PRIMARY KEY (singleton);


--
-- Name: billing_portal_operations billing_portal_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_portal_operations
    ADD CONSTRAINT billing_portal_operations_pkey PRIMARY KEY (idempotency_key);


--
-- Name: billing_products billing_products_code_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_products
    ADD CONSTRAINT billing_products_code_key UNIQUE (code);


--
-- Name: billing_products billing_products_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_products
    ADD CONSTRAINT billing_products_pkey PRIMARY KEY (id);


--
-- Name: billing_uncertain_recoveries billing_uncertain_recoveries_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_uncertain_recoveries
    ADD CONSTRAINT billing_uncertain_recoveries_pkey PRIMARY KEY (idempotency_key);


--
-- Name: billing_workspace_operations billing_workspace_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspace_operations
    ADD CONSTRAINT billing_workspace_operations_pkey PRIMARY KEY (idempotency_key);


--
-- Name: billing_workspace_recoveries billing_workspace_recoveries_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspace_recoveries
    ADD CONSTRAINT billing_workspace_recoveries_pkey PRIMARY KEY (idempotency_key);


--
-- Name: billing_workspace_usage_periods billing_workspace_usage_periods_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspace_usage_periods
    ADD CONSTRAINT billing_workspace_usage_periods_pkey PRIMARY KEY (workspace_id, period_start, period_end);


--
-- Name: billing_workspaces billing_workspaces_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspaces
    ADD CONSTRAINT billing_workspaces_pkey PRIMARY KEY (id);


--
-- Name: billing_workspaces billing_workspaces_provider_customer_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspaces
    ADD CONSTRAINT billing_workspaces_provider_customer_id_key UNIQUE (provider_customer_id);


--
-- Name: billing_workspaces billing_workspaces_provider_subscription_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspaces
    ADD CONSTRAINT billing_workspaces_provider_subscription_id_key UNIQUE (provider_subscription_id);


--
-- Name: billing_workspaces billing_workspaces_team_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspaces
    ADD CONSTRAINT billing_workspaces_team_id_key UNIQUE (team_id);


--
-- Name: billing_workspaces billing_workspaces_user_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspaces
    ADD CONSTRAINT billing_workspaces_user_id_key UNIQUE (user_id);


--
-- Name: browser_access_sessions browser_access_sessions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_access_sessions
    ADD CONSTRAINT browser_access_sessions_pkey PRIMARY KEY (grant_id);


--
-- Name: browser_access_sessions browser_access_sessions_token_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_access_sessions
    ADD CONSTRAINT browser_access_sessions_token_hash_key UNIQUE (token_hash);


--
-- Name: browser_access_transactions browser_access_transactions_handoff_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_access_transactions
    ADD CONSTRAINT browser_access_transactions_handoff_hash_key UNIQUE (handoff_hash);


--
-- Name: browser_access_transactions browser_access_transactions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_access_transactions
    ADD CONSTRAINT browser_access_transactions_pkey PRIMARY KEY (transaction_id);


--
-- Name: browser_machine_credentials browser_machine_credentials_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_machine_credentials
    ADD CONSTRAINT browser_machine_credentials_pkey PRIMARY KEY (credential_id);


--
-- Name: browser_machine_credentials browser_machine_credentials_token_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_machine_credentials
    ADD CONSTRAINT browser_machine_credentials_token_hash_key UNIQUE (token_hash);


--
-- Name: cli_access_tokens cli_access_tokens_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.cli_access_tokens
    ADD CONSTRAINT cli_access_tokens_pkey PRIMARY KEY (token_hash);


--
-- Name: cli_client_sessions cli_client_sessions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.cli_client_sessions
    ADD CONSTRAINT cli_client_sessions_pkey PRIMARY KEY (id);


--
-- Name: cli_refresh_tokens cli_refresh_tokens_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.cli_refresh_tokens
    ADD CONSTRAINT cli_refresh_tokens_pkey PRIMARY KEY (token_hash);


--
-- Name: codex_sessions codex_sessions_cli_client_session_id_idempotency_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.codex_sessions
    ADD CONSTRAINT codex_sessions_cli_client_session_id_idempotency_key_key UNIQUE (cli_client_session_id, idempotency_key);


--
-- Name: codex_sessions codex_sessions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.codex_sessions
    ADD CONSTRAINT codex_sessions_pkey PRIMARY KEY (id);


--
-- Name: connector_proof_replays connector_proof_replays_account_id_tunnel_id_connector_id_p_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.connector_proof_replays
    ADD CONSTRAINT connector_proof_replays_account_id_tunnel_id_connector_id_p_key UNIQUE (account_id, tunnel_id, connector_id, proof_digest);


--
-- Name: connector_proof_replays connector_proof_replays_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.connector_proof_replays
    ADD CONSTRAINT connector_proof_replays_pkey PRIMARY KEY (account_id, tunnel_id, connector_id, proof_kind, nonce);


--
-- Name: control_config_assignments control_config_assignments_environment_id_unique; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_assignments
    ADD CONSTRAINT control_config_assignments_environment_id_unique UNIQUE (environment_id);


--
-- Name: control_config_assignments control_config_assignments_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_assignments
    ADD CONSTRAINT control_config_assignments_id_key UNIQUE (id);


--
-- Name: control_config_assignments control_config_assignments_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_assignments
    ADD CONSTRAINT control_config_assignments_pkey PRIMARY KEY (machine_id);


--
-- Name: control_config_conflict_resolutions control_config_conflict_resol_environment_id_conflict_revis_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_conflict_resolutions
    ADD CONSTRAINT control_config_conflict_resol_environment_id_conflict_revis_key UNIQUE (environment_id, conflict_revision, path);


--
-- Name: control_config_conflict_resolutions control_config_conflict_resolutions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_conflict_resolutions
    ADD CONSTRAINT control_config_conflict_resolutions_pkey PRIMARY KEY (id);


--
-- Name: control_config_credentials control_config_credentials_jti_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_credentials
    ADD CONSTRAINT control_config_credentials_jti_key UNIQUE (jti);


--
-- Name: control_config_credentials control_config_credentials_operation_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_credentials
    ADD CONSTRAINT control_config_credentials_operation_key_key UNIQUE (operation_key);


--
-- Name: control_config_credentials control_config_credentials_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_credentials
    ADD CONSTRAINT control_config_credentials_pkey PRIMARY KEY (jti_hash);


--
-- Name: control_config_repositories control_config_repositories_owner_user_id_provider_external_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_repositories
    ADD CONSTRAINT control_config_repositories_owner_user_id_provider_external_key UNIQUE (owner_user_id, provider, external_ref);


--
-- Name: control_config_repositories control_config_repositories_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_repositories
    ADD CONSTRAINT control_config_repositories_pkey PRIMARY KEY (id);


--
-- Name: control_config_repository_access_operations control_config_repository_access_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_repository_access_operations
    ADD CONSTRAINT control_config_repository_access_operations_pkey PRIMARY KEY (operation_id);


--
-- Name: control_config_repository_lease_authority control_config_repository_lease_authority_lease_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_repository_lease_authority
    ADD CONSTRAINT control_config_repository_lease_authority_lease_id_key UNIQUE (lease_id);


--
-- Name: control_config_repository_lease_authority control_config_repository_lease_authority_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_repository_lease_authority
    ADD CONSTRAINT control_config_repository_lease_authority_pkey PRIMARY KEY (repository_id);


--
-- Name: control_config_repository_lease_operations control_config_repository_lease_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_repository_lease_operations
    ADD CONSTRAINT control_config_repository_lease_operations_pkey PRIMARY KEY (operation_id);


--
-- Name: control_config_repository_migration_reviews control_config_repository_migration_reviews_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_repository_migration_reviews
    ADD CONSTRAINT control_config_repository_migration_reviews_pkey PRIMARY KEY (source, source_id);


--
-- Name: control_config_sync_migration_reviews control_config_sync_migration_reviews_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_sync_migration_reviews
    ADD CONSTRAINT control_config_sync_migration_reviews_pkey PRIMARY KEY (source, project_id, machine_id);


--
-- Name: control_config_sync_status_history control_config_sync_status_history_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_sync_status_history
    ADD CONSTRAINT control_config_sync_status_history_pkey PRIMARY KEY (environment_id, sync_revision);


--
-- Name: control_config_sync_statuses control_config_sync_statuses_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_sync_statuses
    ADD CONSTRAINT control_config_sync_statuses_pkey PRIMARY KEY (environment_id);


--
-- Name: control_connector_generations control_connector_generations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_connector_generations
    ADD CONSTRAINT control_connector_generations_pkey PRIMARY KEY (environment_id, connector_id);


--
-- Name: control_environments control_environments_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_environments
    ADD CONSTRAINT control_environments_pkey PRIMARY KEY (id);


--
-- Name: control_helper_enrollments control_helper_enrollments_jti_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_helper_enrollments
    ADD CONSTRAINT control_helper_enrollments_jti_hash_key UNIQUE (jti_hash);


--
-- Name: control_helper_enrollments control_helper_enrollments_operation_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_helper_enrollments
    ADD CONSTRAINT control_helper_enrollments_operation_key_key UNIQUE (operation_key);


--
-- Name: control_helper_enrollments control_helper_enrollments_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_helper_enrollments
    ADD CONSTRAINT control_helper_enrollments_pkey PRIMARY KEY (id);


--
-- Name: control_helpers control_helpers_environment_id_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_helpers
    ADD CONSTRAINT control_helpers_environment_id_id_key UNIQUE (environment_id, id);


--
-- Name: control_helpers control_helpers_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_helpers
    ADD CONSTRAINT control_helpers_pkey PRIMARY KEY (id);


--
-- Name: control_helpers control_helpers_replacement_operation_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_helpers
    ADD CONSTRAINT control_helpers_replacement_operation_key_key UNIQUE (replacement_operation_key);


--
-- Name: control_operation_recoveries control_operation_recoveries_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_operation_recoveries
    ADD CONSTRAINT control_operation_recoveries_pkey PRIMARY KEY (operation_key);


--
-- Name: control_operations control_operations_operation_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_operations
    ADD CONSTRAINT control_operations_operation_key_key UNIQUE (operation_key);


--
-- Name: control_operations control_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_operations
    ADD CONSTRAINT control_operations_pkey PRIMARY KEY (id);


--
-- Name: control_reconciliation_attempts control_reconciliation_attempts_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_reconciliation_attempts
    ADD CONSTRAINT control_reconciliation_attempts_pkey PRIMARY KEY (id);


--
-- Name: control_route_operations control_route_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_route_operations
    ADD CONSTRAINT control_route_operations_pkey PRIMARY KEY (operation_key);


--
-- Name: control_routes control_routes_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_routes
    ADD CONSTRAINT control_routes_pkey PRIMARY KEY (id);


--
-- Name: control_routes control_routes_public_host_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_routes
    ADD CONSTRAINT control_routes_public_host_key UNIQUE (public_host);


--
-- Name: control_signing_key_revocation_operations control_signing_key_revocation_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_signing_key_revocation_operations
    ADD CONSTRAINT control_signing_key_revocation_operations_pkey PRIMARY KEY (operation_key);


--
-- Name: control_signing_key_revocations control_signing_key_revocations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_signing_key_revocations
    ADD CONSTRAINT control_signing_key_revocations_pkey PRIMARY KEY (key_id);


--
-- Name: control_sync_leases control_sync_leases_machine_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_sync_leases
    ADD CONSTRAINT control_sync_leases_machine_id_key UNIQUE (machine_id);


--
-- Name: control_sync_leases control_sync_leases_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_sync_leases
    ADD CONSTRAINT control_sync_leases_pkey PRIMARY KEY (address_slot);


--
-- Name: control_tunnel_nodes control_tunnel_nodes_id_process_epoch_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_tunnel_nodes
    ADD CONSTRAINT control_tunnel_nodes_id_process_epoch_key UNIQUE (id, process_epoch);


--
-- Name: control_tunnel_nodes control_tunnel_nodes_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_tunnel_nodes
    ADD CONSTRAINT control_tunnel_nodes_pkey PRIMARY KEY (id);


--
-- Name: control_usage_counters control_usage_counters_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_usage_counters
    ADD CONSTRAINT control_usage_counters_pkey PRIMARY KEY (edge_node_id, counter_epoch, environment_id, route_id, direction, authority_id);


--
-- Name: control_usage_key_operations control_usage_key_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_usage_key_operations
    ADD CONSTRAINT control_usage_key_operations_pkey PRIMARY KEY (operation_key);


--
-- Name: control_usage_receipts control_usage_receipts_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_usage_receipts
    ADD CONSTRAINT control_usage_receipts_pkey PRIMARY KEY (operation_id);


--
-- Name: control_usage_verification_keys control_usage_verification_keys_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_usage_verification_keys
    ADD CONSTRAINT control_usage_verification_keys_pkey PRIMARY KEY (key_id);


--
-- Name: device_grants device_grants_device_code_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.device_grants
    ADD CONSTRAINT device_grants_device_code_hash_key UNIQUE (device_code_hash);


--
-- Name: device_grants device_grants_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.device_grants
    ADD CONSTRAINT device_grants_pkey PRIMARY KEY (id);


--
-- Name: device_grants device_grants_user_code_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.device_grants
    ADD CONSTRAINT device_grants_user_code_hash_key UNIQUE (user_code_hash);


--
-- Name: device_service_discovery_policies device_service_discovery_policies_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.device_service_discovery_policies
    ADD CONSTRAINT device_service_discovery_policies_pkey PRIMARY KEY (machine_id);


--
-- Name: device_service_snapshots device_service_snapshots_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.device_service_snapshots
    ADD CONSTRAINT device_service_snapshots_pkey PRIMARY KEY (machine_id);


--
-- Name: diagnostic_upload_intents diagnostic_upload_intents_correlation_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.diagnostic_upload_intents
    ADD CONSTRAINT diagnostic_upload_intents_correlation_id_key UNIQUE (correlation_id);


--
-- Name: diagnostic_upload_intents diagnostic_upload_intents_object_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.diagnostic_upload_intents
    ADD CONSTRAINT diagnostic_upload_intents_object_key_key UNIQUE (object_key);


--
-- Name: diagnostic_upload_intents diagnostic_upload_intents_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.diagnostic_upload_intents
    ADD CONSTRAINT diagnostic_upload_intents_pkey PRIMARY KEY (id);


--
-- Name: diagnostic_upload_intents diagnostic_upload_intents_user_id_operation_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.diagnostic_upload_intents
    ADD CONSTRAINT diagnostic_upload_intents_user_id_operation_key_key UNIQUE (user_id, operation_key);


--
-- Name: edge_dns_provider_budgets edge_dns_provider_budgets_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.edge_dns_provider_budgets
    ADD CONSTRAINT edge_dns_provider_budgets_pkey PRIMARY KEY (zone_id);


--
-- Name: edge_dns_publications edge_dns_publications_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.edge_dns_publications
    ADD CONSTRAINT edge_dns_publications_pkey PRIMARY KEY (hostname);


--
-- Name: environment_authorities environment_authorities_account_id_authority_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_authorities
    ADD CONSTRAINT environment_authorities_account_id_authority_id_key UNIQUE (account_id, authority_id);


--
-- Name: environment_authorities environment_authorities_account_id_operation_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_authorities
    ADD CONSTRAINT environment_authorities_account_id_operation_id_key UNIQUE (account_id, operation_id);


--
-- Name: environment_authorities environment_authorities_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_authorities
    ADD CONSTRAINT environment_authorities_pkey PRIMARY KEY (account_id, generation);


--
-- Name: environment_authority_heads environment_authority_heads_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_authority_heads
    ADD CONSTRAINT environment_authority_heads_pkey PRIMARY KEY (account_id);


--
-- Name: environment_authority_roots environment_authority_roots_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_authority_roots
    ADD CONSTRAINT environment_authority_roots_pkey PRIMARY KEY (account_id, key_id);


--
-- Name: environment_authority_transitions environment_authority_transitions_account_id_operation_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_authority_transitions
    ADD CONSTRAINT environment_authority_transitions_account_id_operation_id_key UNIQUE (account_id, operation_id);


--
-- Name: environment_authority_transitions environment_authority_transitions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_authority_transitions
    ADD CONSTRAINT environment_authority_transitions_pkey PRIMARY KEY (transition_id);


--
-- Name: environment_host_bootstraps environment_host_bootstraps_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_host_bootstraps
    ADD CONSTRAINT environment_host_bootstraps_pkey PRIMARY KEY (account_id, machine_id, recipient_key_id);


--
-- Name: environment_key_bindings environment_key_bindings_account_id_recipient_key_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_key_bindings
    ADD CONSTRAINT environment_key_bindings_account_id_recipient_key_id_key UNIQUE (account_id, recipient_key_id);


--
-- Name: environment_key_bindings environment_key_bindings_account_id_signing_key_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_key_bindings
    ADD CONSTRAINT environment_key_bindings_account_id_signing_key_id_key UNIQUE (account_id, signing_key_id);


--
-- Name: environment_key_bindings environment_key_bindings_account_id_subject_kind_subject_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_key_bindings
    ADD CONSTRAINT environment_key_bindings_account_id_subject_kind_subject_id_key UNIQUE (account_id, subject_kind, subject_id, key_generation);


--
-- Name: environment_key_bindings environment_key_bindings_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_key_bindings
    ADD CONSTRAINT environment_key_bindings_pkey PRIMARY KEY (binding_id);


--
-- Name: environment_key_enrollment_requests environment_key_enrollment_requests_account_id_operation_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_key_enrollment_requests
    ADD CONSTRAINT environment_key_enrollment_requests_account_id_operation_id_key UNIQUE (account_id, operation_id);


--
-- Name: environment_key_enrollment_requests environment_key_enrollment_requests_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_key_enrollment_requests
    ADD CONSTRAINT environment_key_enrollment_requests_pkey PRIMARY KEY (id);


--
-- Name: environment_observations environment_observations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_observations
    ADD CONSTRAINT environment_observations_pkey PRIMARY KEY (machine_id);


--
-- Name: environment_password_vaults environment_password_vaults_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_password_vaults
    ADD CONSTRAINT environment_password_vaults_pkey PRIMARY KEY (account_id);


--
-- Name: environment_scope_manifests environment_scope_manifests_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_scope_manifests
    ADD CONSTRAINT environment_scope_manifests_pkey PRIMARY KEY (scope_id, version);


--
-- Name: environment_scope_manifests environment_scope_manifests_scope_id_manifest_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_scope_manifests
    ADD CONSTRAINT environment_scope_manifests_scope_id_manifest_id_key UNIQUE (scope_id, manifest_id);


--
-- Name: environment_scope_manifests environment_scope_manifests_scope_id_operation_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_scope_manifests
    ADD CONSTRAINT environment_scope_manifests_scope_id_operation_id_key UNIQUE (scope_id, operation_id);


--
-- Name: environment_scope_names environment_scope_names_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_scope_names
    ADD CONSTRAINT environment_scope_names_pkey PRIMARY KEY (scope_id, name);


--
-- Name: environment_scopes environment_scopes_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_scopes
    ADD CONSTRAINT environment_scopes_pkey PRIMARY KEY (id);


--
-- Name: environment_transition_manifests environment_transition_manifests_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_transition_manifests
    ADD CONSTRAINT environment_transition_manifests_pkey PRIMARY KEY (transition_id, scope_ref);


--
-- Name: environment_vault_hosts environment_vault_hosts_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_hosts
    ADD CONSTRAINT environment_vault_hosts_pkey PRIMARY KEY (machine_id);


--
-- Name: environment_vault_operations environment_vault_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_operations
    ADD CONSTRAINT environment_vault_operations_pkey PRIMARY KEY (account_id, operation_id);


--
-- Name: environment_vault_personal_epochs environment_vault_personal_epochs_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_personal_epochs
    ADD CONSTRAINT environment_vault_personal_epochs_pkey PRIMARY KEY (account_id);


--
-- Name: environment_vault_personal_rotations environment_vault_personal_rotation_account_id_operation_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_personal_rotations
    ADD CONSTRAINT environment_vault_personal_rotation_account_id_operation_id_key UNIQUE (account_id, operation_id);


--
-- Name: environment_vault_personal_rotation_scopes environment_vault_personal_rotation_scopes_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_personal_rotation_scopes
    ADD CONSTRAINT environment_vault_personal_rotation_scopes_pkey PRIMARY KEY (account_id, machine_id);


--
-- Name: environment_vault_personal_rotations environment_vault_personal_rotations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_personal_rotations
    ADD CONSTRAINT environment_vault_personal_rotations_pkey PRIMARY KEY (account_id);


--
-- Name: environment_vault_projection_fences environment_vault_projection_fences_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_projection_fences
    ADD CONSTRAINT environment_vault_projection_fences_pkey PRIMARY KEY (machine_id);


--
-- Name: environment_vault_projection_sources environment_vault_projection_sources_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_projection_sources
    ADD CONSTRAINT environment_vault_projection_sources_pkey PRIMARY KEY (machine_id, owner_kind, owner_id, source_machine_id);


--
-- Name: environment_vault_scopes environment_vault_scopes_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_scopes
    ADD CONSTRAINT environment_vault_scopes_pkey PRIMARY KEY (owner_kind, owner_id, machine_id);


--
-- Name: environment_vault_team_grants environment_vault_team_grants_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_team_grants
    ADD CONSTRAINT environment_vault_team_grants_pkey PRIMARY KEY (team_id, account_id);


--
-- Name: environment_vault_team_members environment_vault_team_members_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_team_members
    ADD CONSTRAINT environment_vault_team_members_pkey PRIMARY KEY (team_id, account_id);


--
-- Name: environment_vault_teams environment_vault_teams_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_teams
    ADD CONSTRAINT environment_vault_teams_pkey PRIMARY KEY (team_id);


--
-- Name: github_config_repositories github_config_repositories_owner_name_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.github_config_repositories
    ADD CONSTRAINT github_config_repositories_owner_name_key UNIQUE (owner, name);


--
-- Name: github_config_repositories github_config_repositories_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.github_config_repositories
    ADD CONSTRAINT github_config_repositories_pkey PRIMARY KEY (id);


--
-- Name: github_config_repositories github_config_repositories_user_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.github_config_repositories
    ADD CONSTRAINT github_config_repositories_user_id_key UNIQUE (user_id);


--
-- Name: github_oauth_tokens github_oauth_tokens_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.github_oauth_tokens
    ADD CONSTRAINT github_oauth_tokens_pkey PRIMARY KEY (id);


--
-- Name: github_repo_provisioning_attempts github_repo_provisioning_attempts_idempotency_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.github_repo_provisioning_attempts
    ADD CONSTRAINT github_repo_provisioning_attempts_idempotency_key_key UNIQUE (idempotency_key);


--
-- Name: github_repo_provisioning_attempts github_repo_provisioning_attempts_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.github_repo_provisioning_attempts
    ADD CONSTRAINT github_repo_provisioning_attempts_pkey PRIMARY KEY (id);


--
-- Name: helper_revocation_outbox helper_revocation_outbox_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.helper_revocation_outbox
    ADD CONSTRAINT helper_revocation_outbox_pkey PRIMARY KEY (id);


--
-- Name: helper_identity_renewals hosted_helper_identity_renewals_helper_id_operation_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.helper_identity_renewals
    ADD CONSTRAINT hosted_helper_identity_renewals_helper_id_operation_key_key UNIQUE (helper_id, operation_key);


--
-- Name: helper_identity_renewals hosted_helper_identity_renewals_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.helper_identity_renewals
    ADD CONSTRAINT hosted_helper_identity_renewals_pkey PRIMARY KEY (operation_key);


--
-- Name: inspector_credentials inspector_credentials_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.inspector_credentials
    ADD CONSTRAINT inspector_credentials_pkey PRIMARY KEY (credential_id);


--
-- Name: inspector_credentials inspector_credentials_token_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.inspector_credentials
    ADD CONSTRAINT inspector_credentials_token_hash_key UNIQUE (token_hash);


--
-- Name: lazy_access_policies lazy_access_policies_hostname_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_access_policies
    ADD CONSTRAINT lazy_access_policies_hostname_key UNIQUE (hostname);


--
-- Name: lazy_access_policies lazy_access_policies_id_account_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_access_policies
    ADD CONSTRAINT lazy_access_policies_id_account_id_key UNIQUE (id, account_id);


--
-- Name: lazy_access_policies lazy_access_policies_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_access_policies
    ADD CONSTRAINT lazy_access_policies_pkey PRIMARY KEY (id);


--
-- Name: lazy_activation_waiters lazy_activation_waiters_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_activation_waiters
    ADD CONSTRAINT lazy_activation_waiters_pkey PRIMARY KEY (waiter_id);


--
-- Name: lazy_activations lazy_activations_activation_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_activations
    ADD CONSTRAINT lazy_activations_activation_id_key UNIQUE (activation_id);


--
-- Name: lazy_activations lazy_activations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_activations
    ADD CONSTRAINT lazy_activations_pkey PRIMARY KEY (policy_id);


--
-- Name: lazy_environment_identities lazy_environment_identities_environment_label_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_environment_identities
    ADD CONSTRAINT lazy_environment_identities_environment_label_key UNIQUE (environment_label);


--
-- Name: lazy_environment_identities lazy_environment_identities_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_environment_identities
    ADD CONSTRAINT lazy_environment_identities_pkey PRIMARY KEY (environment_id);


--
-- Name: lazy_runtime_owners lazy_runtime_owners_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_runtime_owners
    ADD CONSTRAINT lazy_runtime_owners_pkey PRIMARY KEY (machine_id);


--
-- Name: machine_control_renewals machine_control_renewals_credential_jti_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_control_renewals
    ADD CONSTRAINT machine_control_renewals_credential_jti_key UNIQUE (credential_jti);


--
-- Name: machine_control_renewals machine_control_renewals_operation_machine_installation_unique; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_control_renewals
    ADD CONSTRAINT machine_control_renewals_operation_machine_installation_unique UNIQUE (operation_id, machine_id, installation_generation);


--
-- Name: machine_control_renewals machine_control_renewals_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_control_renewals
    ADD CONSTRAINT machine_control_renewals_pkey PRIMARY KEY (operation_id);


--
-- Name: machine_control_sessions machine_control_sessions_credential_jti_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_control_sessions
    ADD CONSTRAINT machine_control_sessions_credential_jti_key UNIQUE (credential_jti);


--
-- Name: machine_control_sessions machine_control_sessions_machine_id_session_generation_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_control_sessions
    ADD CONSTRAINT machine_control_sessions_machine_id_session_generation_key UNIQUE (machine_id, session_generation);


--
-- Name: machine_control_sessions machine_control_sessions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_control_sessions
    ADD CONSTRAINT machine_control_sessions_pkey PRIMARY KEY (machine_id);


--
-- Name: machine_ssh_host_key_owners machine_ssh_host_key_owners_fingerprint_user_machine_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_ssh_host_key_owners
    ADD CONSTRAINT machine_ssh_host_key_owners_fingerprint_user_machine_id_key UNIQUE (fingerprint, user_machine_id);


--
-- Name: machine_ssh_host_key_owners machine_ssh_host_key_owners_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_ssh_host_key_owners
    ADD CONSTRAINT machine_ssh_host_key_owners_pkey PRIMARY KEY (fingerprint);


--
-- Name: machine_ssh_host_key_owners machine_ssh_host_key_owners_public_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_ssh_host_key_owners
    ADD CONSTRAINT machine_ssh_host_key_owners_public_key_key UNIQUE (public_key);


--
-- Name: machine_ssh_host_key_sets machine_ssh_host_key_sets_id_user_machine_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_ssh_host_key_sets
    ADD CONSTRAINT machine_ssh_host_key_sets_id_user_machine_id_key UNIQUE (id, user_machine_id);


--
-- Name: machine_ssh_host_key_sets machine_ssh_host_key_sets_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_ssh_host_key_sets
    ADD CONSTRAINT machine_ssh_host_key_sets_pkey PRIMARY KEY (id);


--
-- Name: machine_ssh_host_keys machine_ssh_host_keys_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_ssh_host_keys
    ADD CONSTRAINT machine_ssh_host_keys_pkey PRIMARY KEY (set_id, fingerprint);


--
-- Name: machine_ssh_host_keys machine_ssh_host_keys_set_id_ordinal_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_ssh_host_keys
    ADD CONSTRAINT machine_ssh_host_keys_set_id_ordinal_key UNIQUE (set_id, ordinal);


--
-- Name: machine_ssh_targets machine_ssh_targets_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_ssh_targets
    ADD CONSTRAINT machine_ssh_targets_pkey PRIMARY KEY (user_machine_id);


--
-- Name: managed_node_authority managed_node_authority_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.managed_node_authority
    ADD CONSTRAINT managed_node_authority_pkey PRIMARY KEY (node_id);


--
-- Name: managed_node_authority managed_node_authority_runtime_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.managed_node_authority
    ADD CONSTRAINT managed_node_authority_runtime_hash_key UNIQUE (runtime_hash);


--
-- Name: managed_ssh_client_keys managed_ssh_client_keys_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.managed_ssh_client_keys
    ADD CONSTRAINT managed_ssh_client_keys_pkey PRIMARY KEY (fingerprint);


--
-- Name: managed_ssh_client_keys managed_ssh_client_keys_public_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.managed_ssh_client_keys
    ADD CONSTRAINT managed_ssh_client_keys_public_key_key UNIQUE (public_key);


--
-- Name: managed_ssh_operations managed_ssh_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.managed_ssh_operations
    ADD CONSTRAINT managed_ssh_operations_pkey PRIMARY KEY (operation_id);


--
-- Name: operations operations_account_id_idempotency_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.operations
    ADD CONSTRAINT operations_account_id_idempotency_key_key UNIQUE (account_id, idempotency_key);


--
-- Name: operations operations_id_account_unique; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.operations
    ADD CONSTRAINT operations_id_account_unique UNIQUE (id, account_id);


--
-- Name: operations operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.operations
    ADD CONSTRAINT operations_pkey PRIMARY KEY (id);


--
-- Name: operator_actions operator_actions_actor_user_id_idempotency_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.operator_actions
    ADD CONSTRAINT operator_actions_actor_user_id_idempotency_key_key UNIQUE (actor_user_id, idempotency_key);


--
-- Name: operator_actions operator_actions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.operator_actions
    ADD CONSTRAINT operator_actions_pkey PRIMARY KEY (id);


--
-- Name: operator_authentication_challenges operator_authentication_challenges_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.operator_authentication_challenges
    ADD CONSTRAINT operator_authentication_challenges_pkey PRIMARY KEY (session_id);


--
-- Name: operator_authentication_challenges operator_authentication_challenges_state_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.operator_authentication_challenges
    ADD CONSTRAINT operator_authentication_challenges_state_hash_key UNIQUE (state_hash);


--
-- Name: operator_infrastructure_operations operator_infrastructure_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.operator_infrastructure_operations
    ADD CONSTRAINT operator_infrastructure_operations_pkey PRIMARY KEY (actor_user_id, idempotency_key);


--
-- Name: operator_regions operator_regions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.operator_regions
    ADD CONSTRAINT operator_regions_pkey PRIMARY KEY (id);


--
-- Name: peer_endpoint_certificate_operations peer_endpoint_certificate_ope_user_id_certificate_fingerpri_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_certificate_operations
    ADD CONSTRAINT peer_endpoint_certificate_ope_user_id_certificate_fingerpri_key UNIQUE (user_id, certificate_fingerprint);


--
-- Name: peer_endpoint_certificate_operations peer_endpoint_certificate_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_certificate_operations
    ADD CONSTRAINT peer_endpoint_certificate_operations_pkey PRIMARY KEY (operation_id);


--
-- Name: peer_endpoint_certificate_revocations peer_endpoint_certificate_rev_user_id_certificate_fingerpri_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_certificate_revocations
    ADD CONSTRAINT peer_endpoint_certificate_rev_user_id_certificate_fingerpri_key UNIQUE (user_id, certificate_fingerprint);


--
-- Name: peer_endpoint_certificate_revocations peer_endpoint_certificate_revocations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_certificate_revocations
    ADD CONSTRAINT peer_endpoint_certificate_revocations_pkey PRIMARY KEY (operation_id);


--
-- Name: peer_endpoint_certificates peer_endpoint_certificates_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_certificates
    ADD CONSTRAINT peer_endpoint_certificates_pkey PRIMARY KEY (fingerprint);


--
-- Name: peer_endpoint_certificates peer_endpoint_certificates_user_id_endpoint_id_generation_s_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_certificates
    ADD CONSTRAINT peer_endpoint_certificates_user_id_endpoint_id_generation_s_key UNIQUE (user_id, endpoint_id, generation, serial);


--
-- Name: peer_endpoint_enrollment_requests peer_endpoint_enrollment_requests_operation_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_enrollment_requests
    ADD CONSTRAINT peer_endpoint_enrollment_requests_operation_key_key UNIQUE (operation_key);


--
-- Name: peer_endpoint_enrollment_requests peer_endpoint_enrollment_requests_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_enrollment_requests
    ADD CONSTRAINT peer_endpoint_enrollment_requests_pkey PRIMARY KEY (id);


--
-- Name: peer_network_identities peer_network_identities_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_network_identities
    ADD CONSTRAINT peer_network_identities_pkey PRIMARY KEY (user_id, endpoint_id);


--
-- Name: peer_network_identities peer_network_identities_virtual_address_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_network_identities
    ADD CONSTRAINT peer_network_identities_virtual_address_key UNIQUE (virtual_address);


--
-- Name: peer_network_identities peer_network_identities_wireguard_public_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_network_identities
    ADD CONSTRAINT peer_network_identities_wireguard_public_key_key UNIQUE (wireguard_public_key);


--
-- Name: peer_network_registration_operations peer_network_registration_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_network_registration_operations
    ADD CONSTRAINT peer_network_registration_operations_pkey PRIMARY KEY (operation_id);


--
-- Name: polar_events polar_events_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.polar_events
    ADD CONSTRAINT polar_events_pkey PRIMARY KEY (id);


--
-- Name: polar_events polar_events_provider_event_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.polar_events
    ADD CONSTRAINT polar_events_provider_event_id_key UNIQUE (provider_event_id);


--
-- Name: preview_carrier_attachment_outbox preview_carrier_attachment_outbox_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_carrier_attachment_outbox
    ADD CONSTRAINT preview_carrier_attachment_outbox_pkey PRIMARY KEY (account_id, operation_id, attachment_generation, action);


--
-- Name: preview_domains preview_domains_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_domains
    ADD CONSTRAINT preview_domains_pkey PRIMARY KEY (id);


--
-- Name: preview_lease_carrier_attachments preview_lease_carrier_attachments_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_lease_carrier_attachments
    ADD CONSTRAINT preview_lease_carrier_attachments_pkey PRIMARY KEY (account_id, operation_id);


--
-- Name: preview_lease_create_operations preview_lease_create_operatio_account_id_preview_id_operati_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_lease_create_operations
    ADD CONSTRAINT preview_lease_create_operatio_account_id_preview_id_operati_key UNIQUE (account_id, preview_id, operation_id);


--
-- Name: preview_lease_create_operations preview_lease_create_operations_account_id_operation_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_lease_create_operations
    ADD CONSTRAINT preview_lease_create_operations_account_id_operation_id_key UNIQUE (account_id, operation_id);


--
-- Name: preview_lease_create_operations preview_lease_create_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_lease_create_operations
    ADD CONSTRAINT preview_lease_create_operations_pkey PRIMARY KEY (account_id, preview_id);


--
-- Name: preview_leases preview_leases_endpoint_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_leases
    ADD CONSTRAINT preview_leases_endpoint_id_key UNIQUE (endpoint_id);


--
-- Name: preview_leases preview_leases_id_account_unique; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_leases
    ADD CONSTRAINT preview_leases_id_account_unique UNIQUE (id, account_id);


--
-- Name: preview_leases preview_leases_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_leases
    ADD CONSTRAINT preview_leases_pkey PRIMARY KEY (id);


--
-- Name: provider_events provider_events_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.provider_events
    ADD CONSTRAINT provider_events_pkey PRIMARY KEY (id);


--
-- Name: provider_events provider_events_provider_provider_event_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.provider_events
    ADD CONSTRAINT provider_events_provider_provider_event_id_key UNIQUE (provider, provider_event_id);


--
-- Name: provider_route_cleanup_outbox provider_route_cleanup_outbox_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.provider_route_cleanup_outbox
    ADD CONSTRAINT provider_route_cleanup_outbox_pkey PRIMARY KEY (id);


--
-- Name: provider_route_cleanup_outbox provider_route_cleanup_outbox_project_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.provider_route_cleanup_outbox
    ADD CONSTRAINT provider_route_cleanup_outbox_project_id_key UNIQUE (project_id);


--
-- Name: release_authority_bundles release_authority_bundles_payload_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.release_authority_bundles
    ADD CONSTRAINT release_authority_bundles_payload_hash_key UNIQUE (payload_hash);


--
-- Name: release_authority_bundles release_authority_bundles_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.release_authority_bundles
    ADD CONSTRAINT release_authority_bundles_pkey PRIMARY KEY (id);


--
-- Name: release_authority_bundles release_authority_bundles_release_id_platform_architecture__key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.release_authority_bundles
    ADD CONSTRAINT release_authority_bundles_release_id_platform_architecture__key UNIQUE (release_id, platform, architecture, policy_revision);


--
-- Name: release_authority_import_operations release_authority_import_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.release_authority_import_operations
    ADD CONSTRAINT release_authority_import_operations_pkey PRIMARY KEY (actor_user_id, idempotency_key);


--
-- Name: release_authority_requests release_authority_requests_fulfilled_bundle_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.release_authority_requests
    ADD CONSTRAINT release_authority_requests_fulfilled_bundle_id_key UNIQUE (fulfilled_bundle_id);


--
-- Name: release_authority_requests release_authority_requests_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.release_authority_requests
    ADD CONSTRAINT release_authority_requests_pkey PRIMARY KEY (id);


--
-- Name: release_authority_requests release_authority_requests_requested_by_user_id_idempotency_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.release_authority_requests
    ADD CONSTRAINT release_authority_requests_requested_by_user_id_idempotency_key UNIQUE (requested_by_user_id, idempotency_key);


--
-- Name: selfhost_challenges selfhost_challenges_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_challenges
    ADD CONSTRAINT selfhost_challenges_pkey PRIMARY KEY (hash);


--
-- Name: selfhost_installations selfhost_installations_node_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_installations
    ADD CONSTRAINT selfhost_installations_node_id_key UNIQUE (node_id);


--
-- Name: selfhost_installations selfhost_installations_operator_public_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_installations
    ADD CONSTRAINT selfhost_installations_operator_public_key_key UNIQUE (operator_public_key);


--
-- Name: selfhost_installations selfhost_installations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_installations
    ADD CONSTRAINT selfhost_installations_pkey PRIMARY KEY (id);


--
-- Name: selfhost_installations selfhost_installations_runtime_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_installations
    ADD CONSTRAINT selfhost_installations_runtime_hash_key UNIQUE (runtime_hash);


--
-- Name: selfhost_operator_nonces selfhost_operator_nonces_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_operator_nonces
    ADD CONSTRAINT selfhost_operator_nonces_pkey PRIMARY KEY (installation_id, nonce);


--
-- Name: selfhost_pairing_codes selfhost_pairing_codes_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_pairing_codes
    ADD CONSTRAINT selfhost_pairing_codes_pkey PRIMARY KEY (hash);


--
-- Name: selfhost_pairings selfhost_pairings_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_pairings
    ADD CONSTRAINT selfhost_pairings_pkey PRIMARY KEY (installation_id, account_id);


--
-- Name: selfhost_pool_policies selfhost_pool_policies_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_pool_policies
    ADD CONSTRAINT selfhost_pool_policies_pkey PRIMARY KEY (account_id, capability);


--
-- Name: sessions sessions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);


--
-- Name: sessions sessions_session_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.sessions
    ADD CONSTRAINT sessions_session_hash_key UNIQUE (session_hash);


--
-- Name: stripe_events stripe_events_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.stripe_events
    ADD CONSTRAINT stripe_events_pkey PRIMARY KEY (id);


--
-- Name: support_reports support_reports_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.support_reports
    ADD CONSTRAINT support_reports_pkey PRIMARY KEY (id);


--
-- Name: team_config_default_adoptions team_config_default_adoptions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_config_default_adoptions
    ADD CONSTRAINT team_config_default_adoptions_pkey PRIMARY KEY (account_id);


--
-- Name: team_config_defaults team_config_defaults_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_config_defaults
    ADD CONSTRAINT team_config_defaults_pkey PRIMARY KEY (team_id);


--
-- Name: team_inbox_policies team_inbox_policies_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_policies
    ADD CONSTRAINT team_inbox_policies_pkey PRIMARY KEY (account_id);


--
-- Name: team_inbox_receipt_outbox team_inbox_receipt_outbox_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_receipt_outbox
    ADD CONSTRAINT team_inbox_receipt_outbox_pkey PRIMARY KEY (request_id);


--
-- Name: team_inbox_requests team_inbox_requests_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_requests
    ADD CONSTRAINT team_inbox_requests_pkey PRIMARY KEY (request_id);


--
-- Name: team_inbox_requests team_inbox_requests_sender_account_batch_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_requests
    ADD CONSTRAINT team_inbox_requests_sender_account_batch_id_key UNIQUE (sender_account, batch_id);


--
-- Name: team_inbox_requests team_inbox_requests_sender_account_operation_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_requests
    ADD CONSTRAINT team_inbox_requests_sender_account_operation_id_key UNIQUE (sender_account, operation_id);


--
-- Name: team_inbox_sender_policies team_inbox_sender_policies_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_sender_policies
    ADD CONSTRAINT team_inbox_sender_policies_pkey PRIMARY KEY (recipient_account, sender_account);


--
-- Name: team_invitations team_invitations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_invitations
    ADD CONSTRAINT team_invitations_pkey PRIMARY KEY (invitation_id);


--
-- Name: team_members team_members_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_members
    ADD CONSTRAINT team_members_pkey PRIMARY KEY (team_id, account_id);


--
-- Name: team_network_preferences team_network_preferences_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_network_preferences
    ADD CONSTRAINT team_network_preferences_pkey PRIMARY KEY (team_id);


--
-- Name: team_operations team_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_operations
    ADD CONSTRAINT team_operations_pkey PRIMARY KEY (account_id, operation_id);


--
-- Name: team_receipt_smtp team_receipt_smtp_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_receipt_smtp
    ADD CONSTRAINT team_receipt_smtp_pkey PRIMARY KEY (team_id);


--
-- Name: team_resource_bindings team_resource_bindings_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_resource_bindings
    ADD CONSTRAINT team_resource_bindings_pkey PRIMARY KEY (team_id, resource_kind, resource_id);


--
-- Name: team_resource_bindings team_resource_bindings_terminal_process_generation_check; Type: CHECK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE paperboat.team_resource_bindings
    ADD CONSTRAINT team_resource_bindings_terminal_process_generation_check CHECK ((((resource_kind = 'terminal_session'::text) AND ((NOT active) OR (terminal_process_generation > 0))) OR ((resource_kind <> 'terminal_session'::text) AND (terminal_process_generation IS NULL)))) NOT VALID;


--
-- Name: team_resource_grants team_resource_grants_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_resource_grants
    ADD CONSTRAINT team_resource_grants_pkey PRIMARY KEY (team_id, account_id, resource_kind, resource_id, permission);


--
-- Name: teams teams_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.teams
    ADD CONSTRAINT teams_pkey PRIMARY KEY (team_id);


--
-- Name: tunnel_certificate_edge_distributions tunnel_certificate_edge_distributions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_certificate_edge_distributions
    ADD CONSTRAINT tunnel_certificate_edge_distributions_pkey PRIMARY KEY (certificate_id, edge_node_id, edge_process_epoch);


--
-- Name: tunnel_certificate_issuance_locks tunnel_certificate_issuance_locks_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_certificate_issuance_locks
    ADD CONSTRAINT tunnel_certificate_issuance_locks_pkey PRIMARY KEY (domain_id);


--
-- Name: tunnel_certificate_records tunnel_certificate_records_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_certificate_records
    ADD CONSTRAINT tunnel_certificate_records_pkey PRIMARY KEY (id);


--
-- Name: tunnel_config_generations tunnel_config_generations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_config_generations
    ADD CONSTRAINT tunnel_config_generations_pkey PRIMARY KEY (tunnel_id, generation);


--
-- Name: tunnel_connector_activations tunnel_connector_activations_connector_id_process_generatio_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_activations
    ADD CONSTRAINT tunnel_connector_activations_connector_id_process_generatio_key UNIQUE (connector_id, process_generation);


--
-- Name: tunnel_connector_activations tunnel_connector_activations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_activations
    ADD CONSTRAINT tunnel_connector_activations_pkey PRIMARY KEY (operation_id);


--
-- Name: tunnel_connector_credential_generations tunnel_connector_credential_generat_connector_id_generation_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_credential_generations
    ADD CONSTRAINT tunnel_connector_credential_generat_connector_id_generation_key UNIQUE (connector_id, generation);


--
-- Name: tunnel_connector_credential_generations tunnel_connector_credential_generatio_credential_thumbprint_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_credential_generations
    ADD CONSTRAINT tunnel_connector_credential_generatio_credential_thumbprint_key UNIQUE (credential_thumbprint);


--
-- Name: tunnel_connector_credential_generations tunnel_connector_credential_generations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_credential_generations
    ADD CONSTRAINT tunnel_connector_credential_generations_pkey PRIMARY KEY (id);


--
-- Name: tunnel_connector_enrollments tunnel_connector_enrollments_operation_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_enrollments
    ADD CONSTRAINT tunnel_connector_enrollments_operation_id_key UNIQUE (operation_id);


--
-- Name: tunnel_connector_enrollments tunnel_connector_enrollments_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_enrollments
    ADD CONSTRAINT tunnel_connector_enrollments_pkey PRIMARY KEY (id);


--
-- Name: tunnel_connector_enrollments tunnel_connector_enrollments_token_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_enrollments
    ADD CONSTRAINT tunnel_connector_enrollments_token_hash_key UNIQUE (token_hash);


--
-- Name: tunnel_connector_rotation_targets tunnel_connector_rotation_targets_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_rotation_targets
    ADD CONSTRAINT tunnel_connector_rotation_targets_pkey PRIMARY KEY (operation_id, connector_id);


--
-- Name: tunnel_connector_sessions tunnel_connector_sessions_connector_id_process_generation_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_sessions
    ADD CONSTRAINT tunnel_connector_sessions_connector_id_process_generation_key UNIQUE (connector_id, process_generation);


--
-- Name: tunnel_connector_sessions tunnel_connector_sessions_id_connector_unique; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_sessions
    ADD CONSTRAINT tunnel_connector_sessions_id_connector_unique UNIQUE (id, connector_id);


--
-- Name: tunnel_connector_sessions tunnel_connector_sessions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_sessions
    ADD CONSTRAINT tunnel_connector_sessions_pkey PRIMARY KEY (id);


--
-- Name: tunnel_connectors tunnel_connectors_id_tunnel_host_unique; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connectors
    ADD CONSTRAINT tunnel_connectors_id_tunnel_host_unique UNIQUE (id, tunnel_id, host_id);


--
-- Name: tunnel_connectors tunnel_connectors_id_tunnel_unique; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connectors
    ADD CONSTRAINT tunnel_connectors_id_tunnel_unique UNIQUE (id, tunnel_id);


--
-- Name: tunnel_connectors tunnel_connectors_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connectors
    ADD CONSTRAINT tunnel_connectors_pkey PRIMARY KEY (id);


--
-- Name: tunnel_connectors tunnel_connectors_tunnel_id_host_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connectors
    ADD CONSTRAINT tunnel_connectors_tunnel_id_host_id_key UNIQUE (tunnel_id, host_id);


--
-- Name: tunnel_domains tunnel_domains_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_domains
    ADD CONSTRAINT tunnel_domains_pkey PRIMARY KEY (id);


--
-- Name: tunnel_edge_route_assignments tunnel_edge_route_assignments_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_edge_route_assignments
    ADD CONSTRAINT tunnel_edge_route_assignments_pkey PRIMARY KEY (assignment_id);


--
-- Name: tunnel_log_entries tunnel_log_entries_cursor_sequence_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_log_entries
    ADD CONSTRAINT tunnel_log_entries_cursor_sequence_key UNIQUE (cursor_sequence);


--
-- Name: tunnel_log_entries tunnel_log_entries_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_log_entries
    ADD CONSTRAINT tunnel_log_entries_pkey PRIMARY KEY (id);


--
-- Name: tunnel_platform_certificate_targets tunnel_platform_certificate_targets_hostname_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_platform_certificate_targets
    ADD CONSTRAINT tunnel_platform_certificate_targets_hostname_key UNIQUE (hostname);


--
-- Name: tunnel_platform_certificate_targets tunnel_platform_certificate_targets_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_platform_certificate_targets
    ADD CONSTRAINT tunnel_platform_certificate_targets_pkey PRIMARY KEY (id);


--
-- Name: tunnel_routes tunnel_routes_id_tunnel_unique; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_routes
    ADD CONSTRAINT tunnel_routes_id_tunnel_unique UNIQUE (id, tunnel_id);


--
-- Name: tunnel_routes tunnel_routes_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_routes
    ADD CONSTRAINT tunnel_routes_pkey PRIMARY KEY (id);


--
-- Name: tunnel_routes tunnel_routes_tunnel_id_name_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_routes
    ADD CONSTRAINT tunnel_routes_tunnel_id_name_key UNIQUE (tunnel_id, name);


--
-- Name: tunnels tunnels_account_id_name_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnels
    ADD CONSTRAINT tunnels_account_id_name_key UNIQUE (account_id, name);


--
-- Name: tunnels tunnels_id_account_unique; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnels
    ADD CONSTRAINT tunnels_id_account_unique UNIQUE (id, account_id);


--
-- Name: tunnels tunnels_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnels
    ADD CONSTRAINT tunnels_pkey PRIMARY KEY (id);


--
-- Name: tunnels tunnels_stable_endpoint_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnels
    ADD CONSTRAINT tunnels_stable_endpoint_id_key UNIQUE (stable_endpoint_id);


--
-- Name: tunnels tunnels_stable_endpoint_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnels
    ADD CONSTRAINT tunnels_stable_endpoint_key UNIQUE (stable_endpoint);


--
-- Name: usage_accounting_authorities usage_accounting_authorities_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.usage_accounting_authorities
    ADD CONSTRAINT usage_accounting_authorities_pkey PRIMARY KEY (authority_id, edge_node_id, environment_id, route_id, route_revision);


--
-- Name: user_favorites user_favorites_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_favorites
    ADD CONSTRAINT user_favorites_pkey PRIMARY KEY (user_id, kind, resource_id);


--
-- Name: user_identities user_identities_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_identities
    ADD CONSTRAINT user_identities_pkey PRIMARY KEY (id);


--
-- Name: user_identities user_identities_provider_provider_subject_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_identities
    ADD CONSTRAINT user_identities_provider_provider_subject_key UNIQUE (provider, provider_subject);


--
-- Name: user_machine_access_sessions user_machine_access_sessions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_access_sessions
    ADD CONSTRAINT user_machine_access_sessions_pkey PRIMARY KEY (id);


--
-- Name: user_machine_availability_operations user_machine_availability_ope_user_id_user_machine_id_idemp_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_availability_operations
    ADD CONSTRAINT user_machine_availability_ope_user_id_user_machine_id_idemp_key UNIQUE (user_id, user_machine_id, idempotency_key);


--
-- Name: user_machine_availability_operations user_machine_availability_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_availability_operations
    ADD CONSTRAINT user_machine_availability_operations_pkey PRIMARY KEY (id);


--
-- Name: user_machine_bandwidth_periods user_machine_bandwidth_periods_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_bandwidth_periods
    ADD CONSTRAINT user_machine_bandwidth_periods_pkey PRIMARY KEY (id);


--
-- Name: user_machine_bandwidth_periods user_machine_bandwidth_periods_user_machine_id_period_start_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_bandwidth_periods
    ADD CONSTRAINT user_machine_bandwidth_periods_user_machine_id_period_start_key UNIQUE (user_machine_id, period_start);


--
-- Name: user_machine_bandwidth_topups user_machine_bandwidth_topups_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_bandwidth_topups
    ADD CONSTRAINT user_machine_bandwidth_topups_pkey PRIMARY KEY (id);


--
-- Name: user_machine_bandwidth_topups user_machine_bandwidth_topups_provider_order_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_bandwidth_topups
    ADD CONSTRAINT user_machine_bandwidth_topups_provider_order_id_key UNIQUE (provider_order_id);


--
-- Name: user_machine_capability_operations user_machine_capability_opera_user_id_user_machine_id_idemp_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_capability_operations
    ADD CONSTRAINT user_machine_capability_opera_user_id_user_machine_id_idemp_key UNIQUE (user_id, user_machine_id, idempotency_key);


--
-- Name: user_machine_capability_operations user_machine_capability_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_capability_operations
    ADD CONSTRAINT user_machine_capability_operations_pkey PRIMARY KEY (id);


--
-- Name: user_machine_enrollments user_machine_enrollments_bootstrap_token_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_enrollments
    ADD CONSTRAINT user_machine_enrollments_bootstrap_token_hash_key UNIQUE (bootstrap_token_hash);


--
-- Name: user_machine_enrollments user_machine_enrollments_operation_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_enrollments
    ADD CONSTRAINT user_machine_enrollments_operation_id_key UNIQUE (operation_id);


--
-- Name: user_machine_enrollments user_machine_enrollments_pairing_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_enrollments
    ADD CONSTRAINT user_machine_enrollments_pairing_id_key UNIQUE (pairing_id);


--
-- Name: user_machine_enrollments user_machine_enrollments_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_enrollments
    ADD CONSTRAINT user_machine_enrollments_pkey PRIMARY KEY (id);


--
-- Name: user_machine_enrollments user_machine_enrollments_user_id_idempotency_key_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_enrollments
    ADD CONSTRAINT user_machine_enrollments_user_id_idempotency_key_key UNIQUE (user_id, idempotency_key);


--
-- Name: user_machine_entitlements user_machine_entitlements_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_entitlements
    ADD CONSTRAINT user_machine_entitlements_pkey PRIMARY KEY (id);


--
-- Name: user_machine_entitlements user_machine_entitlements_provider_subscription_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_entitlements
    ADD CONSTRAINT user_machine_entitlements_provider_subscription_id_key UNIQUE (provider_subscription_id);


--
-- Name: user_machine_entitlements user_machine_entitlements_user_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_entitlements
    ADD CONSTRAINT user_machine_entitlements_user_id_key UNIQUE (user_id);


--
-- Name: user_machine_maintenance_approvals user_machine_maintenance_appr_user_id_user_machine_id_idemp_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_maintenance_approvals
    ADD CONSTRAINT user_machine_maintenance_appr_user_id_user_machine_id_idemp_key UNIQUE (user_id, user_machine_id, idempotency_key);


--
-- Name: user_machine_maintenance_approvals user_machine_maintenance_approvals_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_maintenance_approvals
    ADD CONSTRAINT user_machine_maintenance_approvals_pkey PRIMARY KEY (id);


--
-- Name: user_machine_pairings user_machine_pairings_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_pairings
    ADD CONSTRAINT user_machine_pairings_pkey PRIMARY KEY (id);


--
-- Name: user_machine_pairings user_machine_pairings_user_code_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_pairings
    ADD CONSTRAINT user_machine_pairings_user_code_key UNIQUE (user_code);


--
-- Name: user_machine_pairings user_machine_pairings_verifier_hash_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_pairings
    ADD CONSTRAINT user_machine_pairings_verifier_hash_key UNIQUE (verifier_hash);


--
-- Name: user_machine_terminal_session_operations user_machine_terminal_session_operations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_terminal_session_operations
    ADD CONSTRAINT user_machine_terminal_session_operations_pkey PRIMARY KEY (id);


--
-- Name: user_machine_terminal_sessions user_machine_terminal_sessions_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_terminal_sessions
    ADD CONSTRAINT user_machine_terminal_sessions_pkey PRIMARY KEY (id);


--
-- Name: user_machine_update_observations user_machine_update_observations_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_update_observations
    ADD CONSTRAINT user_machine_update_observations_pkey PRIMARY KEY (user_machine_id);


--
-- Name: user_machines user_machines_environment_id_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machines
    ADD CONSTRAINT user_machines_environment_id_key UNIQUE (environment_id);


--
-- Name: user_machines user_machines_id_user_id_unique; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machines
    ADD CONSTRAINT user_machines_id_user_id_unique UNIQUE (id, user_id);


--
-- Name: user_machines user_machines_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machines
    ADD CONSTRAINT user_machines_pkey PRIMARY KEY (id);


--
-- Name: user_transfer_destination_defaults user_transfer_destination_defaults_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_transfer_destination_defaults
    ADD CONSTRAINT user_transfer_destination_defaults_pkey PRIMARY KEY (user_id);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: users users_workos_subject_key; Type: CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.users
    ADD CONSTRAINT users_workos_subject_key UNIQUE (workos_subject);


--
-- Name: account_e2ee_keys_cli_session_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX account_e2ee_keys_cli_session_idx ON paperboat.account_e2ee_keys USING btree (cli_client_session_id) WHERE (cli_client_session_id IS NOT NULL);


--
-- Name: account_e2ee_keys_cli_session_unique; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX account_e2ee_keys_cli_session_unique ON paperboat.account_e2ee_keys USING btree (cli_client_session_id) WHERE (cli_client_session_id IS NOT NULL);


--
-- Name: account_e2ee_keys_machine_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX account_e2ee_keys_machine_idx ON paperboat.account_e2ee_keys USING btree (user_machine_id) WHERE (user_machine_id IS NOT NULL);


--
-- Name: account_e2ee_keys_user_active_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX account_e2ee_keys_user_active_idx ON paperboat.account_e2ee_keys USING btree (user_id, created_at, key_id) WHERE (revoked_at IS NULL);


--
-- Name: audit_events_account_cursor; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX audit_events_account_cursor ON paperboat.audit_events USING btree (account_id, cursor_sequence DESC);


--
-- Name: audit_events_correlation; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX audit_events_correlation ON paperboat.audit_events USING btree (correlation_id, cursor_sequence) WHERE (correlation_id IS NOT NULL);


--
-- Name: audit_events_cursor_sequence; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX audit_events_cursor_sequence ON paperboat.audit_events USING btree (cursor_sequence);


--
-- Name: audit_events_idempotency_scope; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX audit_events_idempotency_scope ON paperboat.audit_events USING btree (event_type, resource_type, resource_id, idempotency_key) WHERE (idempotency_key IS NOT NULL);


--
-- Name: audit_events_team_activity_cursor; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX audit_events_team_activity_cursor ON paperboat.audit_events USING btree (resource_id, cursor_sequence DESC) WHERE (resource_type = 'team'::text);


--
-- Name: audit_events_team_activity_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX audit_events_team_activity_expiry ON paperboat.audit_events USING btree (created_at, id) WHERE (resource_type = 'team'::text);


--
-- Name: auth_enrollment_redemptions_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX auth_enrollment_redemptions_expiry ON paperboat.auth_enrollment_redemptions USING btree (expires_at);


--
-- Name: backup_reports_status; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX backup_reports_status ON paperboat.backup_reports USING btree (database_id, kind, completed_at DESC, run_id DESC);


--
-- Name: billing_portal_operations_user_time; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX billing_portal_operations_user_time ON paperboat.billing_portal_operations USING btree (user_id, created_at DESC);


--
-- Name: billing_products_stripe_price_unique; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX billing_products_stripe_price_unique ON paperboat.billing_products USING btree (provider, provider_price_id);


--
-- Name: billing_uncertain_recoveries_operation; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX billing_uncertain_recoveries_operation ON paperboat.billing_uncertain_recoveries USING btree (operation_kind, operation_id, created_at DESC);


--
-- Name: billing_workspace_operation_pending; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX billing_workspace_operation_pending ON paperboat.billing_workspace_operations USING btree (workspace_id, kind) WHERE (state = ANY (ARRAY['pending'::text, 'uncertain'::text]));


--
-- Name: browser_access_sessions_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX browser_access_sessions_expiry ON paperboat.browser_access_sessions USING btree (expires_at, grant_id) WHERE (revoked_at IS NULL);


--
-- Name: browser_access_sessions_resource; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX browser_access_sessions_resource ON paperboat.browser_access_sessions USING btree (resource_kind, resource_id, hostname) WHERE (revoked_at IS NULL);


--
-- Name: browser_access_transactions_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX browser_access_transactions_expiry ON paperboat.browser_access_transactions USING btree (expires_at, transaction_id);


--
-- Name: browser_machine_credentials_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX browser_machine_credentials_expiry ON paperboat.browser_machine_credentials USING btree (expires_at, credential_id) WHERE (revoked_at IS NULL);


--
-- Name: cli_client_sessions_user_machine; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX cli_client_sessions_user_machine ON paperboat.cli_client_sessions USING btree (user_machine_id, created_at DESC) WHERE (user_machine_id IS NOT NULL);


--
-- Name: codex_sessions_active_limit; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX codex_sessions_active_limit ON paperboat.codex_sessions USING btree (user_id, environment_id, created_at) WHERE (state = ANY (ARRAY['preparing'::text, 'ready'::text, 'reconnecting'::text]));


--
-- Name: codex_sessions_orphan_cleanup; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX codex_sessions_orphan_cleanup ON paperboat.codex_sessions USING btree (lease_expires_at) WHERE (state = ANY (ARRAY['preparing'::text, 'ready'::text, 'reconnecting'::text, 'stopping'::text]));


--
-- Name: connector_proof_replays_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX connector_proof_replays_expiry ON paperboat.connector_proof_replays USING btree (expires_at, account_id, tunnel_id, connector_id);


--
-- Name: control_config_assignments_push_repository; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_config_assignments_push_repository ON paperboat.control_config_assignments USING btree (push_repository_id) WHERE (push_repository_id IS NOT NULL);


--
-- Name: control_config_assignments_repository; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_config_assignments_repository ON paperboat.control_config_assignments USING btree (repository_id) WHERE (repository_id IS NOT NULL);


--
-- Name: control_config_conflict_resolutions_pending; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_config_conflict_resolutions_pending ON paperboat.control_config_conflict_resolutions USING btree (environment_id, assignment_id, requested_at) WHERE (state = 'pending'::text);


--
-- Name: control_config_credentials_active; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_config_credentials_active ON paperboat.control_config_credentials USING btree (environment_id, machine_id, expires_at) WHERE (revoked_at IS NULL);


--
-- Name: control_config_repositories_external_identity; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX control_config_repositories_external_identity ON paperboat.control_config_repositories USING btree (owner_user_id, provider, external_repository_id) WHERE (external_repository_id IS NOT NULL);


--
-- Name: control_config_repository_access_active; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_config_repository_access_active ON paperboat.control_config_repository_access_operations USING btree (environment_id, machine_id, expires_at) WHERE ((state = 'issued'::text) AND (revoked_at IS NULL));


--
-- Name: control_config_repository_access_revoke_pending; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_config_repository_access_revoke_pending ON paperboat.control_config_repository_access_operations USING btree (revoked_at, expires_at) WHERE ((revoked_at IS NOT NULL) AND (provider_revoked_at IS NULL) AND (access_ciphertext IS NOT NULL));


--
-- Name: control_config_repository_lease_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_config_repository_lease_expiry ON paperboat.control_config_repository_lease_authority USING btree (expires_at) WHERE ((lease_id IS NOT NULL) AND (revoked_at IS NULL));


--
-- Name: control_config_repository_lease_operations_repository; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_config_repository_lease_operations_repository ON paperboat.control_config_repository_lease_operations USING btree (repository_id, created_at);


--
-- Name: control_config_sync_statuses_repository_state; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_config_sync_statuses_repository_state ON paperboat.control_config_sync_statuses USING btree (repository_id, state, observed_at);


--
-- Name: control_connector_admission_jti; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX control_connector_admission_jti ON paperboat.control_connector_generations USING btree (admission_jti_hash) WHERE (admission_jti_hash IS NOT NULL);


--
-- Name: control_environments_owner_state; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_environments_owner_state ON paperboat.control_environments USING btree (owner_user_id, desired_state);


--
-- Name: control_environments_workspace_state; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_environments_workspace_state ON paperboat.control_environments USING btree (workspace_id, desired_state);


--
-- Name: control_helper_enrollments_active; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_helper_enrollments_active ON paperboat.control_helper_enrollments USING btree (environment_id, helper_id, expires_at) WHERE (state = 'pending'::text);


--
-- Name: control_helper_enrollments_one_pending_per_environment; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX control_helper_enrollments_one_pending_per_environment ON paperboat.control_helper_enrollments USING btree (environment_id) WHERE ((state = 'pending'::text) AND (revoked_at IS NULL));


--
-- Name: control_helpers_environment_state; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_helpers_environment_state ON paperboat.control_helpers USING btree (environment_id, state);


--
-- Name: control_operations_retry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_operations_retry ON paperboat.control_operations USING btree (next_attempt_at, lease_expires_at, created_at) WHERE (state = ANY (ARRAY['pending'::text, 'running'::text, 'failed'::text, 'uncertain'::text]));


--
-- Name: control_reconciliation_pending; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_reconciliation_pending ON paperboat.control_reconciliation_attempts USING btree (environment_id, desired_version, started_at) WHERE (state = ANY (ARRAY['started'::text, 'uncertain'::text]));


--
-- Name: control_routes_environment; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_routes_environment ON paperboat.control_routes USING btree (environment_id, desired_state);


--
-- Name: control_tunnel_nodes_assignment; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_tunnel_nodes_assignment ON paperboat.control_tunnel_nodes USING btree (edge_pool, state, ready);


--
-- Name: control_tunnel_nodes_peer_relay_disco_key; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX control_tunnel_nodes_peer_relay_disco_key ON paperboat.control_tunnel_nodes USING btree (peer_relay_disco_public_key) WHERE (peer_relay_disco_public_key IS NOT NULL);


--
-- Name: control_tunnel_nodes_peer_relay_virtual_address; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX control_tunnel_nodes_peer_relay_virtual_address ON paperboat.control_tunnel_nodes USING btree (peer_relay_virtual_address) WHERE (peer_relay_virtual_address IS NOT NULL);


--
-- Name: control_tunnel_nodes_peer_relay_wireguard_key; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX control_tunnel_nodes_peer_relay_wireguard_key ON paperboat.control_tunnel_nodes USING btree (peer_relay_wireguard_public_key) WHERE (peer_relay_wireguard_public_key IS NOT NULL);


--
-- Name: control_tunnel_nodes_regional_candidates; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_tunnel_nodes_regional_candidates ON paperboat.control_tunnel_nodes USING btree (state, ready, region, registry_expires_at);


--
-- Name: control_usage_receipts_route_time; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_usage_receipts_route_time ON paperboat.control_usage_receipts USING btree (route_id, created_at);


--
-- Name: control_usage_verification_keys_node_active; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX control_usage_verification_keys_node_active ON paperboat.control_usage_verification_keys USING btree (edge_node_id, expires_at) WHERE (revoked_at IS NULL);


--
-- Name: diagnostic_upload_intents_expiry_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX diagnostic_upload_intents_expiry_idx ON paperboat.diagnostic_upload_intents USING btree (expires_at, id) WHERE (state = 'pending'::text);


--
-- Name: diagnostic_upload_intents_retention_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX diagnostic_upload_intents_retention_idx ON paperboat.diagnostic_upload_intents USING btree (retain_until, id) WHERE (state = ANY (ARRAY['uploaded'::text, 'expired'::text]));


--
-- Name: edge_dns_publications_owner_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX edge_dns_publications_owner_idx ON paperboat.edge_dns_publications USING btree (owner_id, hostname);


--
-- Name: environment_authority_transition_pending_unique; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX environment_authority_transition_pending_unique ON paperboat.environment_authority_transitions USING btree (account_id) WHERE (state = ANY (ARRAY['staged'::text, 'ready'::text]));


--
-- Name: environment_key_enrollment_pending_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX environment_key_enrollment_pending_idx ON paperboat.environment_key_enrollment_requests USING btree (account_id, expires_at, created_at) WHERE (state = 'pending'::text);


--
-- Name: environment_observations_account_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX environment_observations_account_idx ON paperboat.environment_observations USING btree (account_id, received_at DESC);


--
-- Name: environment_scopes_global_unique; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX environment_scopes_global_unique ON paperboat.environment_scopes USING btree (account_id) WHERE (scope = 'global'::text);


--
-- Name: environment_scopes_machine_unique; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX environment_scopes_machine_unique ON paperboat.environment_scopes USING btree (account_id, machine_id) WHERE (scope = 'machine'::text);


--
-- Name: environment_vault_projection_source_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX environment_vault_projection_source_idx ON paperboat.environment_vault_projection_sources USING btree (owner_kind, owner_id, source_machine_id);


--
-- Name: hosted_helper_identity_renewals_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX hosted_helper_identity_renewals_expiry ON paperboat.helper_identity_renewals USING btree (expires_at);


--
-- Name: idx_audit_events_resource; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX idx_audit_events_resource ON paperboat.audit_events USING btree (resource_type, resource_id);


--
-- Name: idx_auth_rate_limits_window; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX idx_auth_rate_limits_window ON paperboat.auth_rate_limits USING btree (window_start);


--
-- Name: idx_cli_access_tokens_session; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX idx_cli_access_tokens_session ON paperboat.cli_access_tokens USING btree (cli_client_session_id);


--
-- Name: idx_cli_client_sessions_user_created; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX idx_cli_client_sessions_user_created ON paperboat.cli_client_sessions USING btree (user_id, created_at DESC);


--
-- Name: idx_cli_refresh_tokens_one_active; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX idx_cli_refresh_tokens_one_active ON paperboat.cli_refresh_tokens USING btree (cli_client_session_id) WHERE (state = 'active'::text);


--
-- Name: idx_device_grants_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX idx_device_grants_expiry ON paperboat.device_grants USING btree (expires_at);


--
-- Name: idx_github_config_repositories_user_id; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX idx_github_config_repositories_user_id ON paperboat.github_config_repositories USING btree (user_id);


--
-- Name: idx_github_oauth_tokens_user_id; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX idx_github_oauth_tokens_user_id ON paperboat.github_oauth_tokens USING btree (user_id);


--
-- Name: idx_github_oauth_tokens_user_unique; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX idx_github_oauth_tokens_user_unique ON paperboat.github_oauth_tokens USING btree (user_id);


--
-- Name: idx_helper_revocation_outbox_pending; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX idx_helper_revocation_outbox_pending ON paperboat.helper_revocation_outbox USING btree (created_at) WHERE (propagated_at IS NULL);


--
-- Name: idx_provider_route_cleanup_outbox_pending; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX idx_provider_route_cleanup_outbox_pending ON paperboat.provider_route_cleanup_outbox USING btree (created_at) WHERE (propagated_at IS NULL);


--
-- Name: idx_sessions_user_id; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX idx_sessions_user_id ON paperboat.sessions USING btree (user_id);


--
-- Name: inspector_credentials_account; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX inspector_credentials_account ON paperboat.inspector_credentials USING btree (account_id, expires_at) WHERE (revoked_at IS NULL);


--
-- Name: inspector_credentials_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX inspector_credentials_expiry ON paperboat.inspector_credentials USING btree (expires_at, credential_id) WHERE (revoked_at IS NULL);


--
-- Name: inspector_native_cli; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX inspector_native_cli ON paperboat.inspector_credentials USING btree (native_cli_session_id) WHERE (revoked_at IS NULL);


--
-- Name: inspector_native_machine; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX inspector_native_machine ON paperboat.inspector_credentials USING btree (native_machine_id) WHERE (revoked_at IS NULL);


--
-- Name: lazy_access_policies_active_target; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX lazy_access_policies_active_target ON paperboat.lazy_access_policies USING btree (environment_id, target_scheme, target_address) WHERE (deleted_at IS NULL);


--
-- Name: lazy_access_policies_environment_active; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX lazy_access_policies_environment_active ON paperboat.lazy_access_policies USING btree (environment_id, id) WHERE (deleted_at IS NULL);


--
-- Name: lazy_activation_waiters_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX lazy_activation_waiters_expiry ON paperboat.lazy_activation_waiters USING btree (expires_at);


--
-- Name: machine_control_renewals_expiry_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX machine_control_renewals_expiry_idx ON paperboat.machine_control_renewals USING btree (expires_at);


--
-- Name: machine_control_renewals_machine_session_generation_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX machine_control_renewals_machine_session_generation_idx ON paperboat.machine_control_renewals USING btree (machine_id, session_generation) WHERE (session_generation IS NOT NULL);


--
-- Name: machine_ssh_host_key_sets_active_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX machine_ssh_host_key_sets_active_idx ON paperboat.machine_ssh_host_key_sets USING btree (user_machine_id) WHERE (state = 'active'::text);


--
-- Name: machine_ssh_host_key_sets_live_fingerprint_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX machine_ssh_host_key_sets_live_fingerprint_idx ON paperboat.machine_ssh_host_key_sets USING btree (user_machine_id, machine_generation, set_fingerprint) WHERE (state = ANY (ARRAY['active'::text, 'pending'::text]));


--
-- Name: machine_ssh_host_key_sets_live_observation_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX machine_ssh_host_key_sets_live_observation_idx ON paperboat.machine_ssh_host_key_sets USING btree (user_machine_id, machine_generation, observation_generation) WHERE (state = ANY (ARRAY['active'::text, 'pending'::text]));


--
-- Name: machine_ssh_host_key_sets_pending_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX machine_ssh_host_key_sets_pending_idx ON paperboat.machine_ssh_host_key_sets USING btree (user_machine_id) WHERE (state = 'pending'::text);


--
-- Name: managed_ssh_client_keys_active_session_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX managed_ssh_client_keys_active_session_idx ON paperboat.managed_ssh_client_keys USING btree (cli_client_session_id) WHERE (state = 'active'::text);


--
-- Name: managed_ssh_client_keys_active_user_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX managed_ssh_client_keys_active_user_idx ON paperboat.managed_ssh_client_keys USING btree (user_id, reconciliation_version) WHERE (state = 'active'::text);


--
-- Name: operations_pending; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX operations_pending ON paperboat.operations USING btree (state, next_retry_at, updated_at, id) WHERE (state = ANY (ARRAY['pending'::text, 'running'::text, 'uncertain'::text]));


--
-- Name: operations_resource; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX operations_resource ON paperboat.operations USING btree (resource_kind, resource_id, created_at DESC, id);


--
-- Name: operator_authentication_challenges_expiry_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX operator_authentication_challenges_expiry_idx ON paperboat.operator_authentication_challenges USING btree (expires_at);


--
-- Name: peer_endpoint_certificates_active_generation_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX peer_endpoint_certificates_active_generation_idx ON paperboat.peer_endpoint_certificates USING btree (user_id, endpoint_id, generation) WHERE (revoked_at IS NULL);


--
-- Name: peer_endpoint_certificates_expiry_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX peer_endpoint_certificates_expiry_idx ON paperboat.peer_endpoint_certificates USING btree (expires_at) WHERE (revoked_at IS NULL);


--
-- Name: peer_endpoint_certificates_key_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX peer_endpoint_certificates_key_idx ON paperboat.peer_endpoint_certificates USING btree (key_id);


--
-- Name: peer_endpoint_enrollment_requests_pending_expiry_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX peer_endpoint_enrollment_requests_pending_expiry_idx ON paperboat.peer_endpoint_enrollment_requests USING btree (expires_at) WHERE (state = 'pending'::text);


--
-- Name: peer_endpoint_enrollment_requests_pending_identity_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX peer_endpoint_enrollment_requests_pending_identity_idx ON paperboat.peer_endpoint_enrollment_requests USING btree (user_id, endpoint_id, generation) WHERE (state = 'pending'::text);


--
-- Name: peer_network_identity_active_endpoint_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX peer_network_identity_active_endpoint_idx ON paperboat.peer_network_identities USING btree (endpoint_id) WHERE (revoked_at IS NULL);


--
-- Name: peer_network_identity_disco_public_key; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX peer_network_identity_disco_public_key ON paperboat.peer_network_identities USING btree (disco_public_key) WHERE (disco_public_key IS NOT NULL);


--
-- Name: preview_carrier_attachment_outbox_pending; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX preview_carrier_attachment_outbox_pending ON paperboat.preview_carrier_attachment_outbox USING btree (next_attempt_at, created_at, account_id, operation_id) WHERE (state = ANY (ARRAY['pending'::text, 'in_flight'::text, 'failed'::text]));


--
-- Name: preview_domains_certificate_due; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX preview_domains_certificate_due ON paperboat.preview_domains USING btree (certificate_expires_at, id) WHERE ((deleted_at IS NULL) AND (certificate_state = ANY (ARRAY['pending'::text, 'issuing'::text, 'ready'::text, 'renewing'::text, 'failed'::text])));


--
-- Name: preview_domains_dns_due; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX preview_domains_dns_due ON paperboat.preview_domains USING btree (dns_next_check_at, id) WHERE ((deleted_at IS NULL) AND (ownership_state = ANY (ARRAY['pending'::text, 'failed'::text, 'verified'::text])));


--
-- Name: preview_domains_live_hostname_unique; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX preview_domains_live_hostname_unique ON paperboat.preview_domains USING btree (hostname) WHERE (deleted_at IS NULL);


--
-- Name: preview_domains_preview_hostname_live_unique; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX preview_domains_preview_hostname_live_unique ON paperboat.preview_domains USING btree (preview_id, hostname) WHERE (deleted_at IS NULL);


--
-- Name: preview_domains_preview_state; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX preview_domains_preview_state ON paperboat.preview_domains USING btree (preview_id, ownership_state, certificate_state, hostname, id);


--
-- Name: preview_domains_quarantine_due; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX preview_domains_quarantine_due ON paperboat.preview_domains USING btree (quarantine_until, hostname, id) WHERE ((deleted_at IS NOT NULL) AND (conflict_state = 'quarantined'::text));


--
-- Name: preview_lease_carrier_attachments_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX preview_lease_carrier_attachments_expiry ON paperboat.preview_lease_carrier_attachments USING btree (expires_at, account_id, operation_id) WHERE (state <> ALL (ARRAY['failed'::text, 'released'::text]));


--
-- Name: preview_lease_carrier_attachments_idempotency; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX preview_lease_carrier_attachments_idempotency ON paperboat.preview_lease_carrier_attachments USING btree (account_id, idempotency_key);


--
-- Name: preview_lease_carrier_attachments_live_preview; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX preview_lease_carrier_attachments_live_preview ON paperboat.preview_lease_carrier_attachments USING btree (account_id, preview_id) WHERE (state <> ALL (ARRAY['failed'::text, 'released'::text]));


--
-- Name: preview_lease_carrier_attachments_session; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX preview_lease_carrier_attachments_session ON paperboat.preview_lease_carrier_attachments USING btree (connector_id, process_generation, connector_session_id) WHERE (state <> ALL (ARRAY['failed'::text, 'released'::text]));


--
-- Name: preview_leases_account_state; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX preview_leases_account_state ON paperboat.preview_leases USING btree (account_id, terminal_state, created_at DESC, id);


--
-- Name: preview_leases_endpoint_reserved; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX preview_leases_endpoint_reserved ON paperboat.preview_leases USING btree (endpoint) WHERE ((terminal_state = 'active'::text) OR (endpoint !~ '^https://p[0-9]+-'::text));


--
-- Name: preview_leases_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX preview_leases_expiry ON paperboat.preview_leases USING btree (LEAST(lease_deadline, COALESCE(user_deadline, lease_deadline)), id) WHERE (terminal_state = 'active'::text);


--
-- Name: preview_leases_owner; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX preview_leases_owner ON paperboat.preview_leases USING btree (owner_device_id, owner_session_id, terminal_state, lease_deadline, id);


--
-- Name: preview_leases_owner_heartbeat; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX preview_leases_owner_heartbeat ON paperboat.preview_leases USING btree (owner_last_seen_at, id) WHERE (terminal_state = 'active'::text);


--
-- Name: release_authority_bundles_latest; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX release_authority_bundles_latest ON paperboat.release_authority_bundles USING btree (release_id, platform, architecture, policy_revision DESC);


--
-- Name: support_reports_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX support_reports_expiry ON paperboat.support_reports USING btree (expires_at);


--
-- Name: support_reports_lookup; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX support_reports_lookup ON paperboat.support_reports USING btree (owner_id, support_reference, created_at, id);


--
-- Name: team_inbox_receipt_outbox_pending; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX team_inbox_receipt_outbox_pending ON paperboat.team_inbox_receipt_outbox USING btree (next_attempt_at, request_id) WHERE (status = ANY (ARRAY['pending'::text, 'failed'::text]));


--
-- Name: team_inbox_requests_recipient_pending; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX team_inbox_requests_recipient_pending ON paperboat.team_inbox_requests USING btree (recipient_account, created_at, request_id) WHERE (status = 'pending'::text);


--
-- Name: team_invitation_history; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX team_invitation_history ON paperboat.team_invitations USING btree (team_id, COALESCE(accepted_at, cancelled_at) DESC, invitation_id DESC) WHERE ((accepted_at IS NOT NULL) OR (cancelled_at IS NOT NULL));


--
-- Name: team_machine_grants_all; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX team_machine_grants_all ON paperboat.team_machine_grants USING btree (team_id, machine_id) WHERE (audience = 'all_members'::text);


--
-- Name: team_machine_grants_selected; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX team_machine_grants_selected ON paperboat.team_machine_grants USING btree (team_id, machine_id, account_id) WHERE (audience = 'selected_member'::text);


--
-- Name: team_operations_recent; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX team_operations_recent ON paperboat.team_operations USING btree (account_id, created_at DESC, operation_id DESC);


--
-- Name: team_pending_invitation; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX team_pending_invitation ON paperboat.team_invitations USING btree (team_id, recipient_account) WHERE ((accepted_at IS NULL) AND (cancelled_at IS NULL));


--
-- Name: team_terminal_session_grants_all; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX team_terminal_session_grants_all ON paperboat.team_terminal_session_grants USING btree (team_id, terminal_session_id) WHERE (audience = 'all_members'::text);


--
-- Name: team_terminal_session_grants_selected; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX team_terminal_session_grants_selected ON paperboat.team_terminal_session_grants USING btree (team_id, terminal_session_id, account_id) WHERE (audience = 'selected_member'::text);


--
-- Name: tunnel_certificate_edge_distributions_live; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_certificate_edge_distributions_live ON paperboat.tunnel_certificate_edge_distributions USING btree (edge_node_id, state, updated_at DESC);


--
-- Name: tunnel_certificate_issuance_locks_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_certificate_issuance_locks_expiry ON paperboat.tunnel_certificate_issuance_locks USING btree (lease_until, domain_id);


--
-- Name: tunnel_certificate_records_account; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_certificate_records_account ON paperboat.tunnel_certificate_records USING btree (account_id, domain_id, certificate_generation DESC);


--
-- Name: tunnel_certificate_records_active_domain_hostname; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX tunnel_certificate_records_active_domain_hostname ON paperboat.tunnel_certificate_records USING btree (domain_id, hostname) WHERE (state = 'active'::text);


--
-- Name: tunnel_certificate_records_domain_hostname_generation; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX tunnel_certificate_records_domain_hostname_generation ON paperboat.tunnel_certificate_records USING btree (domain_id, hostname, certificate_generation);


--
-- Name: tunnel_certificate_records_domain_hostname_state; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_certificate_records_domain_hostname_state ON paperboat.tunnel_certificate_records USING btree (domain_id, hostname, state, certificate_generation DESC);


--
-- Name: tunnel_certificate_records_due; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_certificate_records_due ON paperboat.tunnel_certificate_records USING btree (renewal_at, id) WHERE (state = 'active'::text);


--
-- Name: tunnel_certificate_records_preview_active_hostname; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX tunnel_certificate_records_preview_active_hostname ON paperboat.tunnel_certificate_records USING btree (preview_id, hostname) WHERE ((target_kind = 'preview_lease'::text) AND (state = 'active'::text));


--
-- Name: tunnel_certificate_records_preview_target; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_certificate_records_preview_target ON paperboat.tunnel_certificate_records USING btree (preview_id, hostname, preview_generation, state) WHERE (target_kind = 'preview_lease'::text);


--
-- Name: tunnel_config_generations_content; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX tunnel_config_generations_content ON paperboat.tunnel_config_generations USING btree (tunnel_id, content_hash);


--
-- Name: tunnel_config_generations_one_active; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX tunnel_config_generations_one_active ON paperboat.tunnel_config_generations USING btree (tunnel_id) WHERE (activation_state = 'active'::text);


--
-- Name: tunnel_config_generations_retention; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_config_generations_retention ON paperboat.tunnel_config_generations USING btree (retained_until, tunnel_id, generation) WHERE (activation_state = ANY (ARRAY['superseded'::text, 'rejected'::text]));


--
-- Name: tunnel_connector_activations_connector; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_connector_activations_connector ON paperboat.tunnel_connector_activations USING btree (connector_id, process_generation DESC);


--
-- Name: tunnel_connector_credentials_one_active; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX tunnel_connector_credentials_one_active ON paperboat.tunnel_connector_credential_generations USING btree (connector_id) WHERE (state = 'active'::text);


--
-- Name: tunnel_connector_credentials_source_operation; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_connector_credentials_source_operation ON paperboat.tunnel_connector_credential_generations USING btree (source_operation_id, connector_id, generation) WHERE (source_operation_id IS NOT NULL);


--
-- Name: tunnel_connector_credentials_validity; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_connector_credentials_validity ON paperboat.tunnel_connector_credential_generations USING btree (connector_id, state, valid_until DESC);


--
-- Name: tunnel_connector_enrollments_replay; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_connector_enrollments_replay ON paperboat.tunnel_connector_enrollments USING btree (operation_id, consumed_at);


--
-- Name: tunnel_connector_enrollments_scope; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_connector_enrollments_scope ON paperboat.tunnel_connector_enrollments USING btree (account_id, tunnel_id, host_id, expires_at, id);


--
-- Name: tunnel_connector_rotation_targets_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_connector_rotation_targets_expiry ON paperboat.tunnel_connector_rotation_targets USING btree (overlap_until, state, operation_id, connector_id) WHERE (state = ANY (ARRAY['installed'::text, 'ready'::text, 'revoking'::text]));


--
-- Name: tunnel_connector_rotation_targets_state; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_connector_rotation_targets_state ON paperboat.tunnel_connector_rotation_targets USING btree (account_id, tunnel_id, state, updated_at, connector_id);


--
-- Name: tunnel_connector_sessions_credential_generation; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_connector_sessions_credential_generation ON paperboat.tunnel_connector_sessions USING btree (connector_id, credential_generation, process_generation DESC);


--
-- Name: tunnel_connector_sessions_live; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_connector_sessions_live ON paperboat.tunnel_connector_sessions USING btree (connector_id, lease_deadline, process_generation DESC) WHERE (state = ANY (ARRAY['authenticating'::text, 'ready'::text, 'draining'::text]));


--
-- Name: tunnel_connector_sessions_retention; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_connector_sessions_retention ON paperboat.tunnel_connector_sessions USING btree (retained_until, id) WHERE (state = ANY (ARRAY['disconnected'::text, 'expired'::text]));


--
-- Name: tunnel_connectors_credential_thumbprint; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX tunnel_connectors_credential_thumbprint ON paperboat.tunnel_connectors USING btree (credential_thumbprint);


--
-- Name: tunnel_connectors_tunnel_readiness; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_connectors_tunnel_readiness ON paperboat.tunnel_connectors USING btree (tunnel_id, desired_state, drain_state, last_heartbeat_at DESC, id);


--
-- Name: tunnel_domains_dns_due; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_domains_dns_due ON paperboat.tunnel_domains USING btree (dns_next_check_at, id) WHERE ((deleted_at IS NULL) AND (ownership_state = ANY (ARRAY['pending'::text, 'failed'::text])));


--
-- Name: tunnel_domains_live_hostname_unique; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX tunnel_domains_live_hostname_unique ON paperboat.tunnel_domains USING btree (hostname) WHERE (deleted_at IS NULL);


--
-- Name: tunnel_domains_quarantine_due; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_domains_quarantine_due ON paperboat.tunnel_domains USING btree (quarantine_until, hostname, id) WHERE ((deleted_at IS NOT NULL) AND (conflict_state = 'quarantined'::text));


--
-- Name: tunnel_domains_renewal; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_domains_renewal ON paperboat.tunnel_domains USING btree (certificate_expires_at, id) WHERE ((deleted_at IS NULL) AND (certificate_state = ANY (ARRAY['ready'::text, 'renewing'::text, 'failed'::text])));


--
-- Name: tunnel_domains_tunnel_state; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_domains_tunnel_state ON paperboat.tunnel_domains USING btree (tunnel_id, ownership_state, certificate_state, hostname, id);


--
-- Name: tunnel_domains_verification; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_domains_verification ON paperboat.tunnel_domains USING btree (ownership_state, last_verified_at, id) WHERE ((deleted_at IS NULL) AND (ownership_state = ANY (ARRAY['pending'::text, 'failed'::text])));


--
-- Name: tunnel_edge_route_assignments_connector_live; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_edge_route_assignments_connector_live ON paperboat.tunnel_edge_route_assignments USING btree (connector_id, connector_session_id, connector_process_generation) WHERE (state = ANY (ARRAY['staged'::text, 'active'::text, 'draining'::text]));


--
-- Name: tunnel_edge_route_assignments_node_live; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_edge_route_assignments_node_live ON paperboat.tunnel_edge_route_assignments USING btree (edge_node_id, edge_process_epoch, route_id, assignment_generation) WHERE (state = ANY (ARRAY['staged'::text, 'active'::text, 'draining'::text]));


--
-- Name: tunnel_edge_route_assignments_route_connector_node_active; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX tunnel_edge_route_assignments_route_connector_node_active ON paperboat.tunnel_edge_route_assignments USING btree (route_id, connector_id, edge_node_id) WHERE (state = 'active'::text);


--
-- Name: tunnel_edge_route_assignments_route_connector_node_staged; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX tunnel_edge_route_assignments_route_connector_node_staged ON paperboat.tunnel_edge_route_assignments USING btree (route_id, connector_id, edge_node_id) WHERE (state = 'staged'::text);


--
-- Name: tunnel_edge_route_assignments_route_generation; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX tunnel_edge_route_assignments_route_generation ON paperboat.tunnel_edge_route_assignments USING btree (route_id, assignment_generation);


--
-- Name: tunnel_log_entries_preview_cursor; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_log_entries_preview_cursor ON paperboat.tunnel_log_entries USING btree (account_id, preview_id, cursor_sequence);


--
-- Name: tunnel_log_entries_resource_level_cursor; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_log_entries_resource_level_cursor ON paperboat.tunnel_log_entries USING btree (account_id, tunnel_id, level, cursor_sequence);


--
-- Name: tunnel_log_entries_retention; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_log_entries_retention ON paperboat.tunnel_log_entries USING btree (occurred_at, id);


--
-- Name: tunnel_log_entries_tunnel_cursor; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_log_entries_tunnel_cursor ON paperboat.tunnel_log_entries USING btree (account_id, tunnel_id, cursor_sequence);


--
-- Name: tunnel_platform_certificate_targets_due; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_platform_certificate_targets_due ON paperboat.tunnel_platform_certificate_targets USING btree (desired_state, next_retry_at, id) WHERE (desired_state = 'active'::text);


--
-- Name: tunnel_routes_match; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_routes_match ON paperboat.tunnel_routes USING btree (match_type, match_hostname, wildcard_suffix, path_prefix, priority, id) WHERE (desired_state = 'active'::text);


--
-- Name: tunnel_routes_public_tcp_listener_id; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX tunnel_routes_public_tcp_listener_id ON paperboat.tunnel_routes USING btree (public_tcp_listener_id) WHERE (protocol = 'tcp'::text);


--
-- Name: tunnel_routes_public_tcp_port; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX tunnel_routes_public_tcp_port ON paperboat.tunnel_routes USING btree (public_tcp_port) WHERE ((protocol = 'tcp'::text) AND (desired_state <> 'deleted'::text));


--
-- Name: tunnel_routes_tunnel_state; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnel_routes_tunnel_state ON paperboat.tunnel_routes USING btree (tunnel_id, desired_state, priority, name, id);


--
-- Name: tunnels_account_state_updated; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnels_account_state_updated ON paperboat.tunnels USING btree (account_id, desired_state, updated_at DESC, id);


--
-- Name: tunnels_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnels_expiry ON paperboat.tunnels USING btree (expires_at, id) WHERE ((expires_at IS NOT NULL) AND (desired_state <> 'deleted'::text));


--
-- Name: tunnels_summary; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX tunnels_summary ON paperboat.tunnels USING btree (account_id, summary_code, summary_transitioned_at DESC, id);


--
-- Name: usage_accounting_authority_expiry; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX usage_accounting_authority_expiry ON paperboat.usage_accounting_authorities USING btree (retain_until);


--
-- Name: user_favorites_user_created_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_favorites_user_created_idx ON paperboat.user_favorites USING btree (user_id, created_at, kind, resource_id);


--
-- Name: user_machine_access_sessions_machine_active; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machine_access_sessions_machine_active ON paperboat.user_machine_access_sessions USING btree (user_machine_id, created_at) WHERE (state = 'active'::text);


--
-- Name: user_machine_access_sessions_network_authority_idx; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machine_access_sessions_network_authority_idx ON paperboat.user_machine_access_sessions USING btree (user_id, cli_client_session_id, user_machine_id, expires_at) WHERE ((state = 'active'::text) AND (revoked_at IS NULL));


--
-- Name: user_machine_access_sessions_pending_revocation; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machine_access_sessions_pending_revocation ON paperboat.user_machine_access_sessions USING btree (created_at) WHERE ((state = 'revoked'::text) AND (helper_revoked_at IS NULL));


--
-- Name: user_machine_bandwidth_topups_active; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machine_bandwidth_topups_active ON paperboat.user_machine_bandwidth_topups USING btree (user_id, created_at) WHERE ((state = 'active'::text) AND (remaining_bytes > 0));


--
-- Name: user_machine_enrollments_owner_state; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machine_enrollments_owner_state ON paperboat.user_machine_enrollments USING btree (user_id, state, created_at DESC);


--
-- Name: user_machine_entitlements_active; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machine_entitlements_active ON paperboat.user_machine_entitlements USING btree (user_id, current_period_end) WHERE (state = ANY (ARRAY['trialing'::text, 'active'::text]));


--
-- Name: user_machine_maintenance_approvals_machine; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machine_maintenance_approvals_machine ON paperboat.user_machine_maintenance_approvals USING btree (user_id, user_machine_id, created_at DESC);


--
-- Name: user_machine_maintenance_approvals_pending; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machine_maintenance_approvals_pending ON paperboat.user_machine_maintenance_approvals USING btree (expires_at) WHERE (status = 'pending'::text);


--
-- Name: user_machine_pairings_authenticated_setup_helper_enrollment; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX user_machine_pairings_authenticated_setup_helper_enrollment ON paperboat.user_machine_pairings USING btree (authenticated_setup_helper_enrollment_id) WHERE ((authenticated_setup_helper_enrollment_id IS NOT NULL) AND (state = ANY (ARRAY['approved'::text, 'consumed'::text])));


--
-- Name: user_machine_pairings_authenticated_setup_operation; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX user_machine_pairings_authenticated_setup_operation ON paperboat.user_machine_pairings USING btree (authenticated_setup_cli_session_id, authenticated_setup_operation_id) WHERE (authenticated_setup_cli_session_id IS NOT NULL);


--
-- Name: user_machine_pairings_pending_code; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machine_pairings_pending_code ON paperboat.user_machine_pairings USING btree (user_code, expires_at) WHERE (state = 'pending'::text);


--
-- Name: user_machine_terminal_session_operations_one_pending; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX user_machine_terminal_session_operations_one_pending ON paperboat.user_machine_terminal_session_operations USING btree (terminal_session_id, operation) WHERE (state = 'pending'::text);


--
-- Name: user_machine_terminal_sessions_active_name; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX user_machine_terminal_sessions_active_name ON paperboat.user_machine_terminal_sessions USING btree (user_machine_id, owner_account, lower(name)) WHERE (deleted_at IS NULL);


--
-- Name: user_machine_terminal_sessions_idempotency; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX user_machine_terminal_sessions_idempotency ON paperboat.user_machine_terminal_sessions USING btree (user_machine_id, owner_account, idempotency_key) WHERE (idempotency_key IS NOT NULL);


--
-- Name: user_machine_terminal_sessions_one_default; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX user_machine_terminal_sessions_one_default ON paperboat.user_machine_terminal_sessions USING btree (user_machine_id, owner_account) WHERE (is_default AND (deleted_at IS NULL));


--
-- Name: user_machine_terminal_sessions_terminal; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX user_machine_terminal_sessions_terminal ON paperboat.user_machine_terminal_sessions USING btree (user_machine_id, terminal_id);


--
-- Name: user_machine_update_observations_environment; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machine_update_observations_environment ON paperboat.user_machine_update_observations USING btree (environment_id, observed_at DESC);


--
-- Name: user_machines_active_alias; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX user_machines_active_alias ON paperboat.user_machines USING btree (user_id, lower(alias)) WHERE ((deleted_at IS NULL) AND (owner_team_id IS NULL));


--
-- Name: user_machines_owner_state; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machines_owner_state ON paperboat.user_machines USING btree (user_id, state);


--
-- Name: user_machines_provider_route_client; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machines_provider_route_client ON paperboat.user_machines USING btree (provider_route_client_id) WHERE (provider_route_client_id IS NOT NULL);


--
-- Name: user_machines_public_identity_key; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE UNIQUE INDEX user_machines_public_identity_key ON paperboat.user_machines USING btree (public_identity_key) WHERE ((public_identity_key IS NOT NULL) AND (deleted_at IS NULL));


--
-- Name: user_machines_team_alias; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machines_team_alias ON paperboat.user_machines USING btree (owner_team_id, lower(alias)) WHERE ((deleted_at IS NULL) AND (owner_team_id IS NOT NULL));


--
-- Name: user_machines_team_owner; Type: INDEX; Schema: paperboat; Owner: -
--

CREATE INDEX user_machines_team_owner ON paperboat.user_machines USING btree (owner_team_id) WHERE (owner_team_id IS NOT NULL);


--
-- Name: account_e2ee_keys account_e2ee_keys_revoke_dependents; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER account_e2ee_keys_revoke_dependents AFTER UPDATE OF revoked_at ON paperboat.account_e2ee_keys FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_account_e2ee_key_dependents();


--
-- Name: account_e2ee_roots account_e2ee_roots_revoke_keys; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER account_e2ee_roots_revoke_keys AFTER UPDATE OF revoked_at ON paperboat.account_e2ee_roots FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_account_e2ee_keys_for_root();


--
-- Name: account_e2ee_keys account_key_peer_network_generation; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER account_key_peer_network_generation AFTER UPDATE OF revoked_at ON paperboat.account_e2ee_keys FOR EACH ROW WHEN ((old.revoked_at IS DISTINCT FROM new.revoked_at)) EXECUTE FUNCTION paperboat.advance_peer_network_for_account_key();


--
-- Name: users account_peer_network_generation; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER account_peer_network_generation AFTER UPDATE OF status ON paperboat.users FOR EACH ROW WHEN ((old.status IS DISTINCT FROM new.status)) EXECUTE FUNCTION paperboat.advance_peer_network_for_account();


--
-- Name: account_e2ee_roots account_root_peer_network_generation; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER account_root_peer_network_generation AFTER UPDATE OF revoked_at, generation ON paperboat.account_e2ee_roots FOR EACH ROW EXECUTE FUNCTION paperboat.advance_peer_network_for_account_root();


--
-- Name: audit_events audit_events_append_only; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER audit_events_append_only BEFORE DELETE OR UPDATE ON paperboat.audit_events FOR EACH ROW EXECUTE FUNCTION paperboat.reject_audit_event_mutation();


--
-- Name: billing_workspaces billing_workspace_team_entitlement; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER billing_workspace_team_entitlement AFTER INSERT OR DELETE OR UPDATE ON paperboat.billing_workspaces FOR EACH ROW EXECUTE FUNCTION paperboat.fence_team_paid_entitlement_change();


--
-- Name: peer_endpoint_certificates certificate_peer_network_generation; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER certificate_peer_network_generation AFTER UPDATE OF revoked_at, expires_at ON paperboat.peer_endpoint_certificates FOR EACH ROW EXECUTE FUNCTION paperboat.advance_peer_network_for_certificate();


--
-- Name: cli_client_sessions cli_client_sessions_revoke_e2ee_keys; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER cli_client_sessions_revoke_e2ee_keys AFTER UPDATE OF state ON paperboat.cli_client_sessions FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_account_e2ee_keys_for_cli_session();


--
-- Name: cli_client_sessions cli_session_peer_network_generation; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER cli_session_peer_network_generation AFTER UPDATE OF state, revoked_at ON paperboat.cli_client_sessions FOR EACH ROW EXECUTE FUNCTION paperboat.advance_peer_network_for_cli_session();


--
-- Name: users control_account_config_sync_revocation_trigger; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER control_account_config_sync_revocation_trigger AFTER UPDATE OF status ON paperboat.users FOR EACH ROW EXECUTE FUNCTION paperboat.control_account_config_sync_revocation();


--
-- Name: control_environments control_environment_config_sync_revocation_trigger; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER control_environment_config_sync_revocation_trigger AFTER UPDATE OF desired_state ON paperboat.control_environments FOR EACH ROW EXECUTE FUNCTION paperboat.control_environment_config_sync_revocation();


--
-- Name: control_helpers control_helper_config_sync_revocation_trigger; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER control_helper_config_sync_revocation_trigger AFTER UPDATE OF state ON paperboat.control_helpers FOR EACH ROW EXECUTE FUNCTION paperboat.control_helper_config_sync_revocation();


--
-- Name: control_config_repositories control_repository_config_sync_revocation_trigger; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER control_repository_config_sync_revocation_trigger AFTER UPDATE OF state ON paperboat.control_config_repositories FOR EACH ROW EXECUTE FUNCTION paperboat.control_repository_config_sync_revocation();


--
-- Name: user_machines device_environment_label; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER device_environment_label AFTER INSERT OR UPDATE OF environment_id ON paperboat.user_machines FOR EACH ROW EXECUTE FUNCTION paperboat.ensure_device_environment_label();


--
-- Name: control_environments environment_peer_network_generation; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER environment_peer_network_generation AFTER UPDATE OF desired_state, revoked_at ON paperboat.control_environments FOR EACH ROW EXECUTE FUNCTION paperboat.advance_peer_network_for_environment();


--
-- Name: environment_password_vaults environment_personal_rotation_expiry; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER environment_personal_rotation_expiry AFTER UPDATE OF document_id ON paperboat.environment_password_vaults FOR EACH ROW WHEN ((old.document_id IS DISTINCT FROM new.document_id)) EXECUTE FUNCTION paperboat.expire_environment_personal_rotation();


--
-- Name: peer_network_identities identity_peer_network_generation; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER identity_peer_network_generation AFTER UPDATE OF wireguard_public_key, disco_public_key, key_generation, quic_certificate_fingerprint, revoked_at ON paperboat.peer_network_identities FOR EACH ROW EXECUTE FUNCTION paperboat.advance_peer_network_for_identity();


--
-- Name: user_machines machine_peer_network_generation; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER machine_peer_network_generation AFTER UPDATE OF state, seat_state, revoked_at, deleted_at, installation_generation ON paperboat.user_machines FOR EACH ROW WHEN ((((((old.state IS DISTINCT FROM new.state) OR (old.seat_state IS DISTINCT FROM new.seat_state)) OR (old.revoked_at IS DISTINCT FROM new.revoked_at)) OR (old.deleted_at IS DISTINCT FROM new.deleted_at)) OR (old.installation_generation IS DISTINCT FROM new.installation_generation))) EXECUTE FUNCTION paperboat.advance_peer_network_for_machine();


--
-- Name: peer_network_identities peer_network_local_identity_fence; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER peer_network_local_identity_fence BEFORE UPDATE ON paperboat.peer_network_identities FOR EACH ROW EXECUTE FUNCTION paperboat.fence_peer_network_local_identity();


--
-- Name: preview_domains preview_domains_global_hostname_claim_v1; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER preview_domains_global_hostname_claim_v1 BEFORE INSERT OR UPDATE OF hostname, account_id, deleted_at, quarantine_until ON paperboat.preview_domains FOR EACH ROW EXECUTE FUNCTION paperboat.enforce_preview_tunnel_domain_hostname_claim_v1();


--
-- Name: preview_lease_carrier_attachments preview_lease_carrier_attachment_edge_epoch_guard; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER preview_lease_carrier_attachment_edge_epoch_guard BEFORE INSERT OR UPDATE ON paperboat.preview_lease_carrier_attachments FOR EACH ROW EXECUTE FUNCTION paperboat.preview_lease_carrier_attachment_edge_epoch_guard_v1();


--
-- Name: preview_lease_carrier_attachments preview_lease_carrier_attachment_guard; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER preview_lease_carrier_attachment_guard BEFORE INSERT OR UPDATE ON paperboat.preview_lease_carrier_attachments FOR EACH ROW EXECUTE FUNCTION paperboat.preview_lease_carrier_attachment_guard_v1();


--
-- Name: operations preview_lease_create_operation_relation; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER preview_lease_create_operation_relation AFTER INSERT OR UPDATE OF resource_id, resource_kind, operation_type ON paperboat.operations FOR EACH ROW EXECUTE FUNCTION paperboat.preview_lease_create_operation_relation_v1();


--
-- Name: control_tunnel_nodes shared_443_check; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER shared_443_check AFTER INSERT OR DELETE OR UPDATE OF endpoint_host ON paperboat.control_tunnel_nodes FOR EACH STATEMENT EXECUTE FUNCTION paperboat.check_shared_443_names_v1();


--
-- Name: preview_domains shared_443_check; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER shared_443_check AFTER INSERT OR DELETE OR UPDATE OF hostname, deleted_at, preview_id ON paperboat.preview_domains FOR EACH STATEMENT EXECUTE FUNCTION paperboat.check_shared_443_names_v1();


--
-- Name: preview_leases shared_443_check; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER shared_443_check AFTER INSERT OR DELETE OR UPDATE OF endpoint, terminal_state ON paperboat.preview_leases FOR EACH STATEMENT EXECUTE FUNCTION paperboat.check_shared_443_names_v1();


--
-- Name: tunnel_domains shared_443_check; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER shared_443_check AFTER INSERT OR DELETE OR UPDATE OF hostname, deleted_at, route_id ON paperboat.tunnel_domains FOR EACH STATEMENT EXECUTE FUNCTION paperboat.check_shared_443_names_v1();


--
-- Name: tunnel_routes shared_443_check; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER shared_443_check AFTER INSERT OR DELETE OR UPDATE OF protocol, match_hostname, wildcard_suffix, match_type, desired_state, tunnel_id ON paperboat.tunnel_routes FOR EACH STATEMENT EXECUTE FUNCTION paperboat.check_shared_443_names_v1();


--
-- Name: tunnels shared_443_check; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER shared_443_check AFTER INSERT OR DELETE OR UPDATE OF stable_endpoint, desired_state ON paperboat.tunnels FOR EACH STATEMENT EXECUTE FUNCTION paperboat.check_shared_443_names_v1();


--
-- Name: control_tunnel_nodes shared_443_lock; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER shared_443_lock BEFORE INSERT OR DELETE OR UPDATE OF endpoint_host ON paperboat.control_tunnel_nodes FOR EACH STATEMENT EXECUTE FUNCTION paperboat.lock_shared_443_names_v1();


--
-- Name: preview_domains shared_443_lock; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER shared_443_lock BEFORE INSERT OR DELETE OR UPDATE OF hostname, deleted_at, preview_id ON paperboat.preview_domains FOR EACH STATEMENT EXECUTE FUNCTION paperboat.lock_shared_443_names_v1();


--
-- Name: preview_leases shared_443_lock; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER shared_443_lock BEFORE INSERT OR DELETE OR UPDATE OF endpoint, terminal_state ON paperboat.preview_leases FOR EACH STATEMENT EXECUTE FUNCTION paperboat.lock_shared_443_names_v1();


--
-- Name: tunnel_domains shared_443_lock; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER shared_443_lock BEFORE INSERT OR DELETE OR UPDATE OF hostname, deleted_at, route_id ON paperboat.tunnel_domains FOR EACH STATEMENT EXECUTE FUNCTION paperboat.lock_shared_443_names_v1();


--
-- Name: tunnel_routes shared_443_lock; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER shared_443_lock BEFORE INSERT OR DELETE OR UPDATE OF protocol, match_hostname, wildcard_suffix, match_type, desired_state, tunnel_id ON paperboat.tunnel_routes FOR EACH STATEMENT EXECUTE FUNCTION paperboat.lock_shared_443_names_v1();


--
-- Name: tunnels shared_443_lock; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER shared_443_lock BEFORE INSERT OR DELETE OR UPDATE OF stable_endpoint, desired_state ON paperboat.tunnels FOR EACH STATEMENT EXECUTE FUNCTION paperboat.lock_shared_443_names_v1();


--
-- Name: team_resource_bindings team_machine_access_binding; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER team_machine_access_binding AFTER DELETE OR UPDATE ON paperboat.team_resource_bindings FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_invalid_team_machine_access();


--
-- Name: team_machine_grants team_machine_access_grant; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER team_machine_access_grant AFTER DELETE OR UPDATE ON paperboat.team_machine_grants FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_invalid_team_machine_access();


--
-- Name: team_members team_machine_access_membership; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER team_machine_access_membership AFTER DELETE OR UPDATE ON paperboat.team_members FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_invalid_team_machine_access();


--
-- Name: user_machines team_machine_access_owner; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER team_machine_access_owner AFTER UPDATE OF owner_team_id, configured_capabilities, state, revoked_at, deleted_at ON paperboat.user_machines FOR EACH ROW WHEN (((old.owner_team_id IS DISTINCT FROM new.owner_team_id) OR (old.configured_capabilities IS DISTINCT FROM new.configured_capabilities) OR (old.state IS DISTINCT FROM new.state) OR (old.revoked_at IS DISTINCT FROM new.revoked_at) OR (old.deleted_at IS DISTINCT FROM new.deleted_at))) EXECUTE FUNCTION paperboat.revoke_invalid_team_machine_access();


--
-- Name: teams team_machine_access_team; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER team_machine_access_team AFTER DELETE OR UPDATE ON paperboat.teams FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_invalid_team_machine_access();


--
-- Name: team_resource_bindings terminal_access_binding; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER terminal_access_binding AFTER DELETE OR UPDATE ON paperboat.team_resource_bindings FOR EACH ROW WHEN ((old.resource_kind = 'terminal_session'::text)) EXECUTE FUNCTION paperboat.revoke_invalid_terminal_session_access();


--
-- Name: team_terminal_session_grants terminal_access_grant; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER terminal_access_grant AFTER INSERT OR DELETE OR UPDATE ON paperboat.team_terminal_session_grants FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_invalid_terminal_session_access();


--
-- Name: team_members terminal_access_membership; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER terminal_access_membership AFTER DELETE OR UPDATE ON paperboat.team_members FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_invalid_terminal_session_access();


--
-- Name: user_machines terminal_access_target; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER terminal_access_target AFTER UPDATE OF state, online, revoked_at, deleted_at, configured_capabilities, observed_capabilities ON paperboat.user_machines FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_unavailable_shared_terminal_target();


--
-- Name: teams terminal_access_team; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER terminal_access_team AFTER DELETE OR UPDATE ON paperboat.teams FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_invalid_terminal_session_access();


--
-- Name: user_machine_terminal_sessions terminal_sharing_machine_session; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER terminal_sharing_machine_session AFTER DELETE OR UPDATE ON paperboat.user_machine_terminal_sessions FOR EACH ROW EXECUTE FUNCTION paperboat.end_terminal_sharing_with_session();


--
-- Name: cli_client_sessions trg_cli_client_sessions_revoke_access; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER trg_cli_client_sessions_revoke_access AFTER UPDATE OF state ON paperboat.cli_client_sessions FOR EACH ROW WHEN ((old.state IS DISTINCT FROM new.state)) EXECUTE FUNCTION paperboat.revoke_access_sessions_on_client_revocation();


--
-- Name: cli_client_sessions trg_cli_client_sessions_revoke_managed_ssh_client_keys; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER trg_cli_client_sessions_revoke_managed_ssh_client_keys AFTER UPDATE OF state ON paperboat.cli_client_sessions FOR EACH ROW WHEN (((old.state IS DISTINCT FROM new.state) AND (new.state = 'revoked'::text))) EXECUTE FUNCTION paperboat.revoke_managed_ssh_client_key_on_session_revocation();


--
-- Name: users trg_users_revoke_cli_client_sessions; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER trg_users_revoke_cli_client_sessions AFTER UPDATE OF status ON paperboat.users FOR EACH ROW WHEN ((old.status IS DISTINCT FROM new.status)) EXECUTE FUNCTION paperboat.revoke_user_cli_client_sessions_on_status_change();


--
-- Name: tunnel_certificate_issuance_locks tunnel_certificate_issuance_lock_target_guard_v1; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER tunnel_certificate_issuance_lock_target_guard_v1 BEFORE INSERT OR UPDATE ON paperboat.tunnel_certificate_issuance_locks FOR EACH ROW EXECUTE FUNCTION paperboat.tunnel_certificate_issuance_lock_target_guard_v1();


--
-- Name: tunnel_certificate_records tunnel_certificate_target_guard_v1; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER tunnel_certificate_target_guard_v1 BEFORE INSERT OR UPDATE ON paperboat.tunnel_certificate_records FOR EACH ROW EXECUTE FUNCTION paperboat.tunnel_certificate_target_guard_v1();


--
-- Name: tunnel_certificate_records tunnel_certificate_user_account_fill_v1; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER tunnel_certificate_user_account_fill_v1 BEFORE INSERT OR UPDATE ON paperboat.tunnel_certificate_records FOR EACH ROW EXECUTE FUNCTION paperboat.tunnel_certificate_user_account_fill_v1();


--
-- Name: tunnel_config_generations tunnel_config_generations_immutable; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER tunnel_config_generations_immutable BEFORE UPDATE ON paperboat.tunnel_config_generations FOR EACH ROW EXECUTE FUNCTION paperboat.reject_tunnel_config_identity_mutation();


--
-- Name: tunnel_connector_rotation_targets tunnel_connector_rotation_target_scope; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER tunnel_connector_rotation_target_scope BEFORE INSERT OR UPDATE ON paperboat.tunnel_connector_rotation_targets FOR EACH ROW EXECUTE FUNCTION paperboat.validate_connector_rotation_target_scope();


--
-- Name: tunnel_domains tunnel_domains_global_hostname_claim_v1; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER tunnel_domains_global_hostname_claim_v1 BEFORE INSERT OR UPDATE OF hostname, account_id, deleted_at, quarantine_until ON paperboat.tunnel_domains FOR EACH ROW EXECUTE FUNCTION paperboat.enforce_preview_tunnel_domain_hostname_claim_v1();


--
-- Name: tunnel_routes tunnel_public_listener_capacity_guard; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER tunnel_public_listener_capacity_guard BEFORE INSERT OR UPDATE ON paperboat.tunnel_routes FOR EACH ROW EXECUTE FUNCTION paperboat.tunnel_public_listener_capacity_guard_v1();


--
-- Name: user_machine_access_sessions user_machine_access_peer_network_generation; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER user_machine_access_peer_network_generation AFTER INSERT OR UPDATE OF state, revoked_at, expires_at ON paperboat.user_machine_access_sessions FOR EACH ROW EXECUTE FUNCTION paperboat.advance_peer_network_for_access_session();


--
-- Name: user_machines user_machines_assign_fallback_alias; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER user_machines_assign_fallback_alias BEFORE INSERT ON paperboat.user_machines FOR EACH ROW EXECUTE FUNCTION paperboat.assign_user_machine_fallback_alias();


--
-- Name: user_machines user_machines_owner_alias; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER user_machines_owner_alias BEFORE INSERT OR UPDATE OF alias, owner_team_id, user_id, deleted_at ON paperboat.user_machines FOR EACH ROW EXECUTE FUNCTION paperboat.enforce_user_machine_owner_alias();


--
-- Name: user_machines user_machines_revoke_e2ee_keys; Type: TRIGGER; Schema: paperboat; Owner: -
--

CREATE TRIGGER user_machines_revoke_e2ee_keys AFTER UPDATE OF revoked_at, deleted_at ON paperboat.user_machines FOR EACH ROW EXECUTE FUNCTION paperboat.revoke_account_e2ee_keys_for_machine();


--
-- Name: account_e2ee_keys account_e2ee_keys_cli_client_session_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_e2ee_keys
    ADD CONSTRAINT account_e2ee_keys_cli_client_session_id_fkey FOREIGN KEY (cli_client_session_id) REFERENCES paperboat.cli_client_sessions(id) ON DELETE SET NULL;


--
-- Name: account_e2ee_keys account_e2ee_keys_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_e2ee_keys
    ADD CONSTRAINT account_e2ee_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: account_e2ee_keys account_e2ee_keys_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_e2ee_keys
    ADD CONSTRAINT account_e2ee_keys_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE SET NULL;


--
-- Name: account_e2ee_roots account_e2ee_roots_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_e2ee_roots
    ADD CONSTRAINT account_e2ee_roots_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: account_network_preferences account_network_preferences_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_network_preferences
    ADD CONSTRAINT account_network_preferences_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: account_network_preferences account_network_preferences_selected_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.account_network_preferences
    ADD CONSTRAINT account_network_preferences_selected_team_id_fkey FOREIGN KEY (selected_team_id) REFERENCES paperboat.teams(team_id) ON DELETE SET NULL;


--
-- Name: audit_events audit_events_actor_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.audit_events
    ADD CONSTRAINT audit_events_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES paperboat.users(id);


--
-- Name: auth_enrollment_redemptions auth_enrollment_redemptions_cli_client_session_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.auth_enrollment_redemptions
    ADD CONSTRAINT auth_enrollment_redemptions_cli_client_session_id_fkey FOREIGN KEY (cli_client_session_id) REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE;


--
-- Name: auth_enrollment_redemptions auth_enrollment_redemptions_enrollment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.auth_enrollment_redemptions
    ADD CONSTRAINT auth_enrollment_redemptions_enrollment_id_fkey FOREIGN KEY (enrollment_id) REFERENCES paperboat.user_machine_enrollments(id) ON DELETE CASCADE;


--
-- Name: billing_checkout_reservations billing_checkout_reservations_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_checkout_reservations
    ADD CONSTRAINT billing_checkout_reservations_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id);


--
-- Name: billing_operator_operations billing_operator_operations_actor_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_operator_operations
    ADD CONSTRAINT billing_operator_operations_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES paperboat.users(id);


--
-- Name: billing_portal_operations billing_portal_operations_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_portal_operations
    ADD CONSTRAINT billing_portal_operations_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: billing_uncertain_recoveries billing_uncertain_recoveries_actor_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_uncertain_recoveries
    ADD CONSTRAINT billing_uncertain_recoveries_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES paperboat.users(id) ON DELETE SET NULL;


--
-- Name: billing_workspace_operations billing_workspace_operations_actor_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspace_operations
    ADD CONSTRAINT billing_workspace_operations_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES paperboat.users(id);


--
-- Name: billing_workspace_operations billing_workspace_operations_workspace_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspace_operations
    ADD CONSTRAINT billing_workspace_operations_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES paperboat.billing_workspaces(id);


--
-- Name: billing_workspace_recoveries billing_workspace_recoveries_actor_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspace_recoveries
    ADD CONSTRAINT billing_workspace_recoveries_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES paperboat.users(id);


--
-- Name: billing_workspace_recoveries billing_workspace_recoveries_operation_key_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspace_recoveries
    ADD CONSTRAINT billing_workspace_recoveries_operation_key_fkey FOREIGN KEY (operation_key) REFERENCES paperboat.billing_workspace_operations(idempotency_key);


--
-- Name: billing_workspace_recoveries billing_workspace_recoveries_workspace_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspace_recoveries
    ADD CONSTRAINT billing_workspace_recoveries_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES paperboat.billing_workspaces(id);


--
-- Name: billing_workspace_usage_periods billing_workspace_usage_periods_workspace_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspace_usage_periods
    ADD CONSTRAINT billing_workspace_usage_periods_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES paperboat.billing_workspaces(id);


--
-- Name: billing_workspaces billing_workspaces_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspaces
    ADD CONSTRAINT billing_workspaces_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id);


--
-- Name: billing_workspaces billing_workspaces_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.billing_workspaces
    ADD CONSTRAINT billing_workspaces_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id);


--
-- Name: browser_access_sessions browser_access_sessions_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_access_sessions
    ADD CONSTRAINT browser_access_sessions_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: browser_access_sessions browser_access_sessions_owner_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_access_sessions
    ADD CONSTRAINT browser_access_sessions_owner_account_id_fkey FOREIGN KEY (owner_account_id) REFERENCES paperboat.users(id);


--
-- Name: browser_access_sessions browser_access_sessions_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_access_sessions
    ADD CONSTRAINT browser_access_sessions_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id);


--
-- Name: browser_access_sessions browser_access_sessions_trusted_session_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_access_sessions
    ADD CONSTRAINT browser_access_sessions_trusted_session_id_fkey FOREIGN KEY (trusted_session_id) REFERENCES paperboat.sessions(id) ON DELETE CASCADE;


--
-- Name: browser_access_transactions browser_access_transactions_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_access_transactions
    ADD CONSTRAINT browser_access_transactions_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: browser_access_transactions browser_access_transactions_owner_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_access_transactions
    ADD CONSTRAINT browser_access_transactions_owner_account_id_fkey FOREIGN KEY (owner_account_id) REFERENCES paperboat.users(id);


--
-- Name: browser_access_transactions browser_access_transactions_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_access_transactions
    ADD CONSTRAINT browser_access_transactions_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id);


--
-- Name: browser_access_transactions browser_access_transactions_trusted_session_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_access_transactions
    ADD CONSTRAINT browser_access_transactions_trusted_session_id_fkey FOREIGN KEY (trusted_session_id) REFERENCES paperboat.sessions(id) ON DELETE CASCADE;


--
-- Name: browser_machine_credentials browser_machine_credentials_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_machine_credentials
    ADD CONSTRAINT browser_machine_credentials_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: browser_machine_credentials browser_machine_credentials_owner_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_machine_credentials
    ADD CONSTRAINT browser_machine_credentials_owner_account_id_fkey FOREIGN KEY (owner_account_id) REFERENCES paperboat.users(id);


--
-- Name: browser_machine_credentials browser_machine_credentials_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.browser_machine_credentials
    ADD CONSTRAINT browser_machine_credentials_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id);


--
-- Name: cli_access_tokens cli_access_tokens_cli_client_session_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.cli_access_tokens
    ADD CONSTRAINT cli_access_tokens_cli_client_session_id_fkey FOREIGN KEY (cli_client_session_id) REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE;


--
-- Name: cli_client_sessions cli_client_sessions_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.cli_client_sessions
    ADD CONSTRAINT cli_client_sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id);


--
-- Name: cli_client_sessions cli_client_sessions_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.cli_client_sessions
    ADD CONSTRAINT cli_client_sessions_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE SET NULL;


--
-- Name: cli_refresh_tokens cli_refresh_tokens_cli_client_session_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.cli_refresh_tokens
    ADD CONSTRAINT cli_refresh_tokens_cli_client_session_id_fkey FOREIGN KEY (cli_client_session_id) REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE;


--
-- Name: codex_sessions codex_sessions_cli_client_session_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.codex_sessions
    ADD CONSTRAINT codex_sessions_cli_client_session_id_fkey FOREIGN KEY (cli_client_session_id) REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE;


--
-- Name: codex_sessions codex_sessions_environment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.codex_sessions
    ADD CONSTRAINT codex_sessions_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE CASCADE;


--
-- Name: codex_sessions codex_sessions_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.codex_sessions
    ADD CONSTRAINT codex_sessions_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: codex_sessions codex_sessions_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.codex_sessions
    ADD CONSTRAINT codex_sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: connector_proof_replays connector_proof_replays_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.connector_proof_replays
    ADD CONSTRAINT connector_proof_replays_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: connector_proof_replays connector_proof_replays_connector_id_tunnel_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.connector_proof_replays
    ADD CONSTRAINT connector_proof_replays_connector_id_tunnel_id_fkey FOREIGN KEY (connector_id, tunnel_id) REFERENCES paperboat.tunnel_connectors(id, tunnel_id) ON DELETE CASCADE;


--
-- Name: connector_proof_replays connector_proof_replays_tunnel_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.connector_proof_replays
    ADD CONSTRAINT connector_proof_replays_tunnel_id_account_id_fkey FOREIGN KEY (tunnel_id, account_id) REFERENCES paperboat.tunnels(id, account_id) ON DELETE CASCADE;


--
-- Name: control_config_assignments control_config_assignments_adopted_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_assignments
    ADD CONSTRAINT control_config_assignments_adopted_team_id_fkey FOREIGN KEY (adopted_team_id) REFERENCES paperboat.teams(team_id) ON DELETE SET NULL;


--
-- Name: control_config_assignments control_config_assignments_environment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_assignments
    ADD CONSTRAINT control_config_assignments_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE CASCADE;


--
-- Name: control_config_assignments control_config_assignments_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_assignments
    ADD CONSTRAINT control_config_assignments_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: control_config_assignments control_config_assignments_push_repository_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_assignments
    ADD CONSTRAINT control_config_assignments_push_repository_id_fkey FOREIGN KEY (push_repository_id) REFERENCES paperboat.control_config_repositories(id) ON DELETE SET NULL;


--
-- Name: control_config_assignments control_config_assignments_repository_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_assignments
    ADD CONSTRAINT control_config_assignments_repository_id_fkey FOREIGN KEY (repository_id) REFERENCES paperboat.control_config_repositories(id) ON DELETE SET NULL;


--
-- Name: control_config_conflict_resolutions control_config_conflict_resolutions_environment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_conflict_resolutions
    ADD CONSTRAINT control_config_conflict_resolutions_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE CASCADE;


--
-- Name: control_config_conflict_resolutions control_config_conflict_resolutions_repository_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_conflict_resolutions
    ADD CONSTRAINT control_config_conflict_resolutions_repository_id_fkey FOREIGN KEY (repository_id) REFERENCES paperboat.control_config_repositories(id) ON DELETE CASCADE;


--
-- Name: control_config_conflict_resolutions control_config_conflict_resolutions_requested_by_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_conflict_resolutions
    ADD CONSTRAINT control_config_conflict_resolutions_requested_by_user_id_fkey FOREIGN KEY (requested_by_user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: control_config_credentials control_config_credentials_environment_id_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_credentials
    ADD CONSTRAINT control_config_credentials_environment_id_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: control_config_repositories control_config_repositories_owner_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_repositories
    ADD CONSTRAINT control_config_repositories_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: control_config_repository_access_operations control_config_repository_access_operations_repository_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_repository_access_operations
    ADD CONSTRAINT control_config_repository_access_operations_repository_id_fkey FOREIGN KEY (repository_id) REFERENCES paperboat.control_config_repositories(id) ON DELETE CASCADE;


--
-- Name: control_config_repository_lease_authority control_config_repository_lease_authority_repository_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_repository_lease_authority
    ADD CONSTRAINT control_config_repository_lease_authority_repository_id_fkey FOREIGN KEY (repository_id) REFERENCES paperboat.control_config_repositories(id) ON DELETE CASCADE;


--
-- Name: control_config_repository_lease_operations control_config_repository_lease_operations_repository_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_repository_lease_operations
    ADD CONSTRAINT control_config_repository_lease_operations_repository_id_fkey FOREIGN KEY (repository_id) REFERENCES paperboat.control_config_repositories(id) ON DELETE CASCADE;


--
-- Name: control_config_repository_migration_reviews control_config_repository_migration_reviews_repository_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_repository_migration_reviews
    ADD CONSTRAINT control_config_repository_migration_reviews_repository_id_fkey FOREIGN KEY (repository_id) REFERENCES paperboat.control_config_repositories(id) ON DELETE SET NULL;


--
-- Name: control_config_sync_migration_reviews control_config_sync_migration_reviews_environment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_sync_migration_reviews
    ADD CONSTRAINT control_config_sync_migration_reviews_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE SET NULL;


--
-- Name: control_config_sync_status_history control_config_sync_status_history_environment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_sync_status_history
    ADD CONSTRAINT control_config_sync_status_history_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE CASCADE;


--
-- Name: control_config_sync_statuses control_config_sync_statuses_environment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_sync_statuses
    ADD CONSTRAINT control_config_sync_statuses_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE CASCADE;


--
-- Name: control_config_sync_statuses control_config_sync_statuses_repository_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_config_sync_statuses
    ADD CONSTRAINT control_config_sync_statuses_repository_id_fkey FOREIGN KEY (repository_id) REFERENCES paperboat.control_config_repositories(id) ON DELETE CASCADE;


--
-- Name: control_connector_generations control_connector_generations_edge_node_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_connector_generations
    ADD CONSTRAINT control_connector_generations_edge_node_id_fkey FOREIGN KEY (edge_node_id) REFERENCES paperboat.control_tunnel_nodes(id) ON DELETE SET NULL;


--
-- Name: control_connector_generations control_connector_generations_environment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_connector_generations
    ADD CONSTRAINT control_connector_generations_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE CASCADE;


--
-- Name: control_environments control_environments_owner_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_environments
    ADD CONSTRAINT control_environments_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES paperboat.users(id) ON DELETE SET NULL;


--
-- Name: control_helper_enrollments control_helper_enrollments_environment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_helper_enrollments
    ADD CONSTRAINT control_helper_enrollments_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE CASCADE;


--
-- Name: control_helper_enrollments control_helper_enrollments_helper_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_helper_enrollments
    ADD CONSTRAINT control_helper_enrollments_helper_id_fkey FOREIGN KEY (helper_id) REFERENCES paperboat.control_helpers(id) ON DELETE CASCADE;


--
-- Name: control_helpers control_helpers_environment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_helpers
    ADD CONSTRAINT control_helpers_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE CASCADE;


--
-- Name: control_operation_recoveries control_operation_recoveries_actor_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_operation_recoveries
    ADD CONSTRAINT control_operation_recoveries_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES paperboat.users(id) ON DELETE SET NULL;


--
-- Name: control_operation_recoveries control_operation_recoveries_operation_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_operation_recoveries
    ADD CONSTRAINT control_operation_recoveries_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES paperboat.control_operations(id) ON DELETE CASCADE;


--
-- Name: control_reconciliation_attempts control_reconciliation_attempts_environment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_reconciliation_attempts
    ADD CONSTRAINT control_reconciliation_attempts_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE CASCADE;


--
-- Name: control_reconciliation_attempts control_reconciliation_attempts_operation_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_reconciliation_attempts
    ADD CONSTRAINT control_reconciliation_attempts_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES paperboat.control_operations(id) ON DELETE SET NULL;


--
-- Name: control_routes control_routes_applied_node_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_routes
    ADD CONSTRAINT control_routes_applied_node_id_fkey FOREIGN KEY (applied_node_id) REFERENCES paperboat.control_tunnel_nodes(id) ON DELETE SET NULL;


--
-- Name: control_routes control_routes_environment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_routes
    ADD CONSTRAINT control_routes_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE CASCADE;


--
-- Name: control_signing_key_revocations control_signing_key_revocations_actor_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_signing_key_revocations
    ADD CONSTRAINT control_signing_key_revocations_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES paperboat.users(id) ON DELETE SET NULL;


--
-- Name: control_sync_leases control_sync_leases_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_sync_leases
    ADD CONSTRAINT control_sync_leases_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE SET NULL;


--
-- Name: control_usage_counters control_usage_counters_edge_node_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_usage_counters
    ADD CONSTRAINT control_usage_counters_edge_node_id_fkey FOREIGN KEY (edge_node_id) REFERENCES paperboat.control_tunnel_nodes(id) ON DELETE CASCADE;


--
-- Name: control_usage_counters control_usage_counters_environment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_usage_counters
    ADD CONSTRAINT control_usage_counters_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE CASCADE;


--
-- Name: control_usage_verification_keys control_usage_verification_keys_edge_node_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.control_usage_verification_keys
    ADD CONSTRAINT control_usage_verification_keys_edge_node_id_fkey FOREIGN KEY (edge_node_id) REFERENCES paperboat.control_tunnel_nodes(id) ON DELETE CASCADE;


--
-- Name: device_grants device_grants_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.device_grants
    ADD CONSTRAINT device_grants_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id);


--
-- Name: device_service_discovery_policies device_service_discovery_policies_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.device_service_discovery_policies
    ADD CONSTRAINT device_service_discovery_policies_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: device_service_snapshots device_service_snapshots_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.device_service_snapshots
    ADD CONSTRAINT device_service_snapshots_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: diagnostic_upload_intents diagnostic_upload_intents_cli_client_session_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.diagnostic_upload_intents
    ADD CONSTRAINT diagnostic_upload_intents_cli_client_session_id_fkey FOREIGN KEY (cli_client_session_id) REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE;


--
-- Name: diagnostic_upload_intents diagnostic_upload_intents_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.diagnostic_upload_intents
    ADD CONSTRAINT diagnostic_upload_intents_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_authorities environment_authorities_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_authorities
    ADD CONSTRAINT environment_authorities_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_authority_heads environment_authority_heads_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_authority_heads
    ADD CONSTRAINT environment_authority_heads_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_authority_roots environment_authority_roots_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_authority_roots
    ADD CONSTRAINT environment_authority_roots_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_authority_transitions environment_authority_transitions_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_authority_transitions
    ADD CONSTRAINT environment_authority_transitions_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_host_bootstraps environment_host_bootstraps_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_host_bootstraps
    ADD CONSTRAINT environment_host_bootstraps_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_host_bootstraps environment_host_bootstraps_machine_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_host_bootstraps
    ADD CONSTRAINT environment_host_bootstraps_machine_id_account_id_fkey FOREIGN KEY (machine_id, account_id) REFERENCES paperboat.user_machines(id, user_id) ON DELETE CASCADE;


--
-- Name: environment_key_bindings environment_key_bindings_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_key_bindings
    ADD CONSTRAINT environment_key_bindings_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_key_enrollment_requests environment_key_enrollment_requests_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_key_enrollment_requests
    ADD CONSTRAINT environment_key_enrollment_requests_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_observations environment_observations_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_observations
    ADD CONSTRAINT environment_observations_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_observations environment_observations_machine_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_observations
    ADD CONSTRAINT environment_observations_machine_id_account_id_fkey FOREIGN KEY (machine_id, account_id) REFERENCES paperboat.user_machines(id, user_id) ON DELETE CASCADE;


--
-- Name: environment_observations environment_observations_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_observations
    ADD CONSTRAINT environment_observations_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: environment_password_vaults environment_password_vaults_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_password_vaults
    ADD CONSTRAINT environment_password_vaults_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_scope_manifests environment_scope_manifests_scope_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_scope_manifests
    ADD CONSTRAINT environment_scope_manifests_scope_id_fkey FOREIGN KEY (scope_id) REFERENCES paperboat.environment_scopes(id) ON DELETE CASCADE;


--
-- Name: environment_scope_names environment_scope_names_scope_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_scope_names
    ADD CONSTRAINT environment_scope_names_scope_id_fkey FOREIGN KEY (scope_id) REFERENCES paperboat.environment_scopes(id) ON DELETE CASCADE;


--
-- Name: environment_scopes environment_scopes_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_scopes
    ADD CONSTRAINT environment_scopes_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_scopes environment_scopes_machine_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_scopes
    ADD CONSTRAINT environment_scopes_machine_id_account_id_fkey FOREIGN KEY (machine_id, account_id) REFERENCES paperboat.user_machines(id, user_id) ON DELETE CASCADE;


--
-- Name: environment_transition_manifests environment_transition_manifests_transition_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_transition_manifests
    ADD CONSTRAINT environment_transition_manifests_transition_id_fkey FOREIGN KEY (transition_id) REFERENCES paperboat.environment_authority_transitions(transition_id) ON DELETE CASCADE;


--
-- Name: environment_vault_hosts environment_vault_hosts_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_hosts
    ADD CONSTRAINT environment_vault_hosts_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_vault_hosts environment_vault_hosts_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_hosts
    ADD CONSTRAINT environment_vault_hosts_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: environment_vault_operations environment_vault_operations_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_operations
    ADD CONSTRAINT environment_vault_operations_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_vault_personal_epochs environment_vault_personal_epochs_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_personal_epochs
    ADD CONSTRAINT environment_vault_personal_epochs_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_vault_personal_rotation_scopes environment_vault_personal_rotatio_account_id_operation_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_personal_rotation_scopes
    ADD CONSTRAINT environment_vault_personal_rotatio_account_id_operation_id_fkey FOREIGN KEY (account_id, operation_id) REFERENCES paperboat.environment_vault_personal_rotations(account_id, operation_id) ON DELETE CASCADE;


--
-- Name: environment_vault_personal_rotations environment_vault_personal_rotations_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_personal_rotations
    ADD CONSTRAINT environment_vault_personal_rotations_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: environment_vault_projection_fences environment_vault_projection_fences_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_projection_fences
    ADD CONSTRAINT environment_vault_projection_fences_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: environment_vault_projection_sources environment_vault_projection_sources_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_projection_sources
    ADD CONSTRAINT environment_vault_projection_sources_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: environment_vault_team_grants environment_vault_team_grants_team_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_team_grants
    ADD CONSTRAINT environment_vault_team_grants_team_id_account_id_fkey FOREIGN KEY (team_id, account_id) REFERENCES paperboat.environment_vault_team_members(team_id, account_id) ON DELETE CASCADE;


--
-- Name: environment_vault_team_members environment_vault_team_members_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_team_members
    ADD CONSTRAINT environment_vault_team_members_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id);


--
-- Name: environment_vault_team_members environment_vault_team_members_team_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_team_members
    ADD CONSTRAINT environment_vault_team_members_team_id_account_id_fkey FOREIGN KEY (team_id, account_id) REFERENCES paperboat.team_members(team_id, account_id) ON DELETE CASCADE;


--
-- Name: environment_vault_team_members environment_vault_team_members_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_team_members
    ADD CONSTRAINT environment_vault_team_members_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.environment_vault_teams(team_id) ON DELETE CASCADE;


--
-- Name: environment_vault_teams environment_vault_teams_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.environment_vault_teams
    ADD CONSTRAINT environment_vault_teams_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id) ON DELETE CASCADE;


--
-- Name: github_config_repositories github_config_repositories_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.github_config_repositories
    ADD CONSTRAINT github_config_repositories_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id);


--
-- Name: github_oauth_tokens github_oauth_tokens_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.github_oauth_tokens
    ADD CONSTRAINT github_oauth_tokens_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id);


--
-- Name: github_repo_provisioning_attempts github_repo_provisioning_attempts_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.github_repo_provisioning_attempts
    ADD CONSTRAINT github_repo_provisioning_attempts_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id);


--
-- Name: helper_identity_renewals hosted_helper_identity_renewals_environment_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.helper_identity_renewals
    ADD CONSTRAINT hosted_helper_identity_renewals_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE CASCADE;


--
-- Name: helper_identity_renewals hosted_helper_identity_renewals_helper_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.helper_identity_renewals
    ADD CONSTRAINT hosted_helper_identity_renewals_helper_id_fkey FOREIGN KEY (helper_id) REFERENCES paperboat.control_helpers(id) ON DELETE CASCADE;


--
-- Name: inspector_credentials inspector_credentials_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.inspector_credentials
    ADD CONSTRAINT inspector_credentials_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: inspector_credentials inspector_credentials_native_cli_session_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.inspector_credentials
    ADD CONSTRAINT inspector_credentials_native_cli_session_id_fkey FOREIGN KEY (native_cli_session_id) REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE;


--
-- Name: inspector_credentials inspector_credentials_native_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.inspector_credentials
    ADD CONSTRAINT inspector_credentials_native_machine_id_fkey FOREIGN KEY (native_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: inspector_credentials inspector_credentials_owner_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.inspector_credentials
    ADD CONSTRAINT inspector_credentials_owner_account_id_fkey FOREIGN KEY (owner_account_id) REFERENCES paperboat.users(id);


--
-- Name: inspector_credentials inspector_credentials_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.inspector_credentials
    ADD CONSTRAINT inspector_credentials_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id) ON DELETE CASCADE;


--
-- Name: lazy_access_policies lazy_access_policies_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_access_policies
    ADD CONSTRAINT lazy_access_policies_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id);


--
-- Name: lazy_access_policies lazy_access_policies_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_access_policies
    ADD CONSTRAINT lazy_access_policies_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id);


--
-- Name: lazy_activation_waiters lazy_activation_waiters_activation_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_activation_waiters
    ADD CONSTRAINT lazy_activation_waiters_activation_id_fkey FOREIGN KEY (activation_id) REFERENCES paperboat.lazy_activations(activation_id) ON UPDATE CASCADE ON DELETE CASCADE;


--
-- Name: lazy_activations lazy_activations_policy_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_activations
    ADD CONSTRAINT lazy_activations_policy_id_fkey FOREIGN KEY (policy_id) REFERENCES paperboat.lazy_access_policies(id);


--
-- Name: lazy_activations lazy_activations_preview_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_activations
    ADD CONSTRAINT lazy_activations_preview_id_fkey FOREIGN KEY (preview_id) REFERENCES paperboat.preview_leases(id);


--
-- Name: lazy_runtime_owners lazy_runtime_owners_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_runtime_owners
    ADD CONSTRAINT lazy_runtime_owners_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id);


--
-- Name: lazy_runtime_owners lazy_runtime_owners_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.lazy_runtime_owners
    ADD CONSTRAINT lazy_runtime_owners_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: machine_control_renewals machine_control_renewals_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_control_renewals
    ADD CONSTRAINT machine_control_renewals_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: machine_control_sessions machine_control_sessions_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_control_sessions
    ADD CONSTRAINT machine_control_sessions_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: machine_control_sessions machine_control_sessions_operation_id_machine_id_installat_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_control_sessions
    ADD CONSTRAINT machine_control_sessions_operation_id_machine_id_installat_fkey FOREIGN KEY (operation_id, machine_id, installation_generation) REFERENCES paperboat.machine_control_renewals(operation_id, machine_id, installation_generation) ON DELETE RESTRICT;


--
-- Name: machine_ssh_host_key_owners machine_ssh_host_key_owners_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_ssh_host_key_owners
    ADD CONSTRAINT machine_ssh_host_key_owners_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: machine_ssh_host_key_sets machine_ssh_host_key_sets_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_ssh_host_key_sets
    ADD CONSTRAINT machine_ssh_host_key_sets_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: machine_ssh_host_keys machine_ssh_host_keys_fingerprint_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_ssh_host_keys
    ADD CONSTRAINT machine_ssh_host_keys_fingerprint_user_machine_id_fkey FOREIGN KEY (fingerprint, user_machine_id) REFERENCES paperboat.machine_ssh_host_key_owners(fingerprint, user_machine_id) ON DELETE RESTRICT;


--
-- Name: machine_ssh_host_keys machine_ssh_host_keys_set_id_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_ssh_host_keys
    ADD CONSTRAINT machine_ssh_host_keys_set_id_user_machine_id_fkey FOREIGN KEY (set_id, user_machine_id) REFERENCES paperboat.machine_ssh_host_key_sets(id, user_machine_id) ON DELETE CASCADE;


--
-- Name: machine_ssh_targets machine_ssh_targets_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.machine_ssh_targets
    ADD CONSTRAINT machine_ssh_targets_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: managed_node_authority managed_node_authority_node_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.managed_node_authority
    ADD CONSTRAINT managed_node_authority_node_id_fkey FOREIGN KEY (node_id) REFERENCES paperboat.control_tunnel_nodes(id) ON DELETE CASCADE;


--
-- Name: managed_node_authority managed_node_authority_region_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.managed_node_authority
    ADD CONSTRAINT managed_node_authority_region_id_fkey FOREIGN KEY (region_id) REFERENCES paperboat.operator_regions(id);


--
-- Name: managed_ssh_client_keys managed_ssh_client_keys_cli_client_session_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.managed_ssh_client_keys
    ADD CONSTRAINT managed_ssh_client_keys_cli_client_session_id_fkey FOREIGN KEY (cli_client_session_id) REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE;


--
-- Name: managed_ssh_client_keys managed_ssh_client_keys_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.managed_ssh_client_keys
    ADD CONSTRAINT managed_ssh_client_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: managed_ssh_operations managed_ssh_operations_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.managed_ssh_operations
    ADD CONSTRAINT managed_ssh_operations_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: operations operations_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.operations
    ADD CONSTRAINT operations_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: operator_actions operator_actions_actor_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.operator_actions
    ADD CONSTRAINT operator_actions_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES paperboat.users(id);


--
-- Name: operator_authentication_challenges operator_authentication_challenges_session_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.operator_authentication_challenges
    ADD CONSTRAINT operator_authentication_challenges_session_id_fkey FOREIGN KEY (session_id) REFERENCES paperboat.sessions(id) ON DELETE CASCADE;


--
-- Name: operator_infrastructure_operations operator_infrastructure_operations_actor_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.operator_infrastructure_operations
    ADD CONSTRAINT operator_infrastructure_operations_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES paperboat.users(id);


--
-- Name: peer_endpoint_certificate_operations peer_endpoint_certificate_operatio_certificate_fingerprint_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_certificate_operations
    ADD CONSTRAINT peer_endpoint_certificate_operatio_certificate_fingerprint_fkey FOREIGN KEY (certificate_fingerprint) REFERENCES paperboat.peer_endpoint_certificates(fingerprint) ON DELETE RESTRICT;


--
-- Name: peer_endpoint_certificate_operations peer_endpoint_certificate_operations_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_certificate_operations
    ADD CONSTRAINT peer_endpoint_certificate_operations_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.account_e2ee_roots(user_id) ON DELETE CASCADE;


--
-- Name: peer_endpoint_certificate_revocations peer_endpoint_certificate_revocati_certificate_fingerprint_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_certificate_revocations
    ADD CONSTRAINT peer_endpoint_certificate_revocati_certificate_fingerprint_fkey FOREIGN KEY (certificate_fingerprint) REFERENCES paperboat.peer_endpoint_certificates(fingerprint) ON DELETE RESTRICT;


--
-- Name: peer_endpoint_certificate_revocations peer_endpoint_certificate_revocations_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_certificate_revocations
    ADD CONSTRAINT peer_endpoint_certificate_revocations_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.account_e2ee_roots(user_id) ON DELETE CASCADE;


--
-- Name: peer_endpoint_certificates peer_endpoint_certificates_key_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_certificates
    ADD CONSTRAINT peer_endpoint_certificates_key_id_fkey FOREIGN KEY (key_id) REFERENCES paperboat.account_e2ee_keys(key_id) ON DELETE RESTRICT;


--
-- Name: peer_endpoint_certificates peer_endpoint_certificates_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_certificates
    ADD CONSTRAINT peer_endpoint_certificates_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.account_e2ee_roots(user_id) ON DELETE CASCADE;


--
-- Name: peer_endpoint_enrollment_requests peer_endpoint_enrollment_requests_certificate_fingerprint_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_enrollment_requests
    ADD CONSTRAINT peer_endpoint_enrollment_requests_certificate_fingerprint_fkey FOREIGN KEY (certificate_fingerprint) REFERENCES paperboat.peer_endpoint_certificates(fingerprint) ON DELETE SET NULL;


--
-- Name: peer_endpoint_enrollment_requests peer_endpoint_enrollment_requests_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_endpoint_enrollment_requests
    ADD CONSTRAINT peer_endpoint_enrollment_requests_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: peer_network_identities peer_network_identities_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_network_identities
    ADD CONSTRAINT peer_network_identities_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: peer_network_identities peer_network_identities_quic_certificate_fingerprint_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_network_identities
    ADD CONSTRAINT peer_network_identities_quic_certificate_fingerprint_fkey FOREIGN KEY (quic_certificate_fingerprint) REFERENCES paperboat.peer_endpoint_certificates(fingerprint) ON DELETE RESTRICT;


--
-- Name: peer_network_identities peer_network_identities_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_network_identities
    ADD CONSTRAINT peer_network_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: peer_network_registration_operations peer_network_registration_operations_user_id_endpoint_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.peer_network_registration_operations
    ADD CONSTRAINT peer_network_registration_operations_user_id_endpoint_id_fkey FOREIGN KEY (user_id, endpoint_id) REFERENCES paperboat.peer_network_identities(user_id, endpoint_id) ON DELETE CASCADE;


--
-- Name: preview_carrier_attachment_outbox preview_carrier_attachment_outbox_account_id_operation_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_carrier_attachment_outbox
    ADD CONSTRAINT preview_carrier_attachment_outbox_account_id_operation_id_fkey FOREIGN KEY (account_id, operation_id) REFERENCES paperboat.preview_lease_carrier_attachments(account_id, operation_id) ON DELETE CASCADE;


--
-- Name: preview_domains preview_domains_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_domains
    ADD CONSTRAINT preview_domains_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: preview_domains preview_domains_preview_account_fk; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_domains
    ADD CONSTRAINT preview_domains_preview_account_fk FOREIGN KEY (preview_id, account_id) REFERENCES paperboat.preview_leases(id, account_id) ON DELETE CASCADE;


--
-- Name: preview_lease_carrier_attachments preview_lease_carrier_attachm_account_id_preview_id_operat_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_lease_carrier_attachments
    ADD CONSTRAINT preview_lease_carrier_attachm_account_id_preview_id_operat_fkey FOREIGN KEY (account_id, preview_id, operation_id) REFERENCES paperboat.preview_lease_create_operations(account_id, preview_id, operation_id) ON DELETE CASCADE;


--
-- Name: preview_lease_carrier_attachments preview_lease_carrier_attachments_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_lease_carrier_attachments
    ADD CONSTRAINT preview_lease_carrier_attachments_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: preview_lease_carrier_attachments preview_lease_carrier_attachments_edge_node_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_lease_carrier_attachments
    ADD CONSTRAINT preview_lease_carrier_attachments_edge_node_id_fkey FOREIGN KEY (edge_node_id) REFERENCES paperboat.control_tunnel_nodes(id) ON DELETE RESTRICT;


--
-- Name: preview_lease_carrier_attachments preview_lease_carrier_attachments_operation_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_lease_carrier_attachments
    ADD CONSTRAINT preview_lease_carrier_attachments_operation_id_account_id_fkey FOREIGN KEY (operation_id, account_id) REFERENCES paperboat.operations(id, account_id) ON DELETE CASCADE;


--
-- Name: preview_lease_carrier_attachments preview_lease_carrier_attachments_preview_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_lease_carrier_attachments
    ADD CONSTRAINT preview_lease_carrier_attachments_preview_id_account_id_fkey FOREIGN KEY (preview_id, account_id) REFERENCES paperboat.preview_leases(id, account_id) ON DELETE CASCADE;


--
-- Name: preview_lease_create_operations preview_lease_create_operations_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_lease_create_operations
    ADD CONSTRAINT preview_lease_create_operations_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: preview_lease_create_operations preview_lease_create_operations_operation_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_lease_create_operations
    ADD CONSTRAINT preview_lease_create_operations_operation_id_account_id_fkey FOREIGN KEY (operation_id, account_id) REFERENCES paperboat.operations(id, account_id) ON DELETE CASCADE;


--
-- Name: preview_lease_create_operations preview_lease_create_operations_preview_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_lease_create_operations
    ADD CONSTRAINT preview_lease_create_operations_preview_id_account_id_fkey FOREIGN KEY (preview_id, account_id) REFERENCES paperboat.preview_leases(id, account_id) ON DELETE CASCADE;


--
-- Name: preview_leases preview_leases_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.preview_leases
    ADD CONSTRAINT preview_leases_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: release_authority_bundles release_authority_bundles_imported_by_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.release_authority_bundles
    ADD CONSTRAINT release_authority_bundles_imported_by_user_id_fkey FOREIGN KEY (imported_by_user_id) REFERENCES paperboat.users(id) ON DELETE RESTRICT;


--
-- Name: release_authority_bundles release_authority_bundles_request_fk; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.release_authority_bundles
    ADD CONSTRAINT release_authority_bundles_request_fk FOREIGN KEY (authority_request_id) REFERENCES paperboat.release_authority_requests(id) ON DELETE RESTRICT;


--
-- Name: release_authority_import_operations release_authority_import_operations_actor_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.release_authority_import_operations
    ADD CONSTRAINT release_authority_import_operations_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES paperboat.users(id) ON DELETE RESTRICT;


--
-- Name: release_authority_import_operations release_authority_import_operations_bundle_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.release_authority_import_operations
    ADD CONSTRAINT release_authority_import_operations_bundle_id_fkey FOREIGN KEY (bundle_id) REFERENCES paperboat.release_authority_bundles(id) ON DELETE RESTRICT;


--
-- Name: release_authority_requests release_authority_requests_requested_by_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.release_authority_requests
    ADD CONSTRAINT release_authority_requests_requested_by_user_id_fkey FOREIGN KEY (requested_by_user_id) REFERENCES paperboat.users(id) ON DELETE RESTRICT;


--
-- Name: selfhost_installations selfhost_installations_node_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_installations
    ADD CONSTRAINT selfhost_installations_node_id_fkey FOREIGN KEY (node_id) REFERENCES paperboat.control_tunnel_nodes(id);


--
-- Name: selfhost_operator_nonces selfhost_operator_nonces_installation_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_operator_nonces
    ADD CONSTRAINT selfhost_operator_nonces_installation_id_fkey FOREIGN KEY (installation_id) REFERENCES paperboat.selfhost_installations(id) ON DELETE CASCADE;


--
-- Name: selfhost_pairing_codes selfhost_pairing_codes_installation_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_pairing_codes
    ADD CONSTRAINT selfhost_pairing_codes_installation_id_fkey FOREIGN KEY (installation_id) REFERENCES paperboat.selfhost_installations(id);


--
-- Name: selfhost_pairing_codes selfhost_pairing_codes_redeemed_by_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_pairing_codes
    ADD CONSTRAINT selfhost_pairing_codes_redeemed_by_fkey FOREIGN KEY (redeemed_by) REFERENCES paperboat.users(id);


--
-- Name: selfhost_pairings selfhost_pairings_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_pairings
    ADD CONSTRAINT selfhost_pairings_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: selfhost_pairings selfhost_pairings_installation_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_pairings
    ADD CONSTRAINT selfhost_pairings_installation_id_fkey FOREIGN KEY (installation_id) REFERENCES paperboat.selfhost_installations(id);


--
-- Name: selfhost_pool_policies selfhost_pool_policies_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.selfhost_pool_policies
    ADD CONSTRAINT selfhost_pool_policies_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: sessions sessions_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.sessions
    ADD CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id);


--
-- Name: team_config_default_adoptions team_config_default_adoptions_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_config_default_adoptions
    ADD CONSTRAINT team_config_default_adoptions_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: team_config_default_adoptions team_config_default_adoptions_repository_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_config_default_adoptions
    ADD CONSTRAINT team_config_default_adoptions_repository_id_fkey FOREIGN KEY (repository_id) REFERENCES paperboat.control_config_repositories(id) ON DELETE CASCADE;


--
-- Name: team_config_default_adoptions team_config_default_adoptions_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_config_default_adoptions
    ADD CONSTRAINT team_config_default_adoptions_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.team_config_defaults(team_id) ON DELETE CASCADE;


--
-- Name: team_config_defaults team_config_defaults_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_config_defaults
    ADD CONSTRAINT team_config_defaults_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id) ON DELETE CASCADE;


--
-- Name: team_config_defaults team_config_defaults_updated_by_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_config_defaults
    ADD CONSTRAINT team_config_defaults_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES paperboat.users(id);


--
-- Name: team_inbox_policies team_inbox_policies_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_policies
    ADD CONSTRAINT team_inbox_policies_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: team_inbox_receipt_outbox team_inbox_receipt_outbox_recipient_account_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_receipt_outbox
    ADD CONSTRAINT team_inbox_receipt_outbox_recipient_account_fkey FOREIGN KEY (recipient_account) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: team_inbox_receipt_outbox team_inbox_receipt_outbox_request_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_receipt_outbox
    ADD CONSTRAINT team_inbox_receipt_outbox_request_id_fkey FOREIGN KEY (request_id) REFERENCES paperboat.team_inbox_requests(request_id) ON DELETE CASCADE;


--
-- Name: team_inbox_requests team_inbox_requests_destination_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_requests
    ADD CONSTRAINT team_inbox_requests_destination_machine_id_fkey FOREIGN KEY (destination_machine_id) REFERENCES paperboat.user_machines(id);


--
-- Name: team_inbox_requests team_inbox_requests_recipient_account_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_requests
    ADD CONSTRAINT team_inbox_requests_recipient_account_fkey FOREIGN KEY (recipient_account) REFERENCES paperboat.users(id);


--
-- Name: team_inbox_requests team_inbox_requests_sender_account_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_requests
    ADD CONSTRAINT team_inbox_requests_sender_account_fkey FOREIGN KEY (sender_account) REFERENCES paperboat.users(id);


--
-- Name: team_inbox_requests team_inbox_requests_source_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_requests
    ADD CONSTRAINT team_inbox_requests_source_machine_id_fkey FOREIGN KEY (source_machine_id) REFERENCES paperboat.user_machines(id);


--
-- Name: team_inbox_requests team_inbox_requests_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_requests
    ADD CONSTRAINT team_inbox_requests_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id);


--
-- Name: team_inbox_sender_policies team_inbox_sender_policies_recipient_account_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_sender_policies
    ADD CONSTRAINT team_inbox_sender_policies_recipient_account_fkey FOREIGN KEY (recipient_account) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: team_inbox_sender_policies team_inbox_sender_policies_sender_account_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_inbox_sender_policies
    ADD CONSTRAINT team_inbox_sender_policies_sender_account_fkey FOREIGN KEY (sender_account) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: team_invitations team_invitations_created_by_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_invitations
    ADD CONSTRAINT team_invitations_created_by_fkey FOREIGN KEY (created_by) REFERENCES paperboat.users(id);


--
-- Name: team_invitations team_invitations_recipient_account_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_invitations
    ADD CONSTRAINT team_invitations_recipient_account_fkey FOREIGN KEY (recipient_account) REFERENCES paperboat.users(id);


--
-- Name: team_invitations team_invitations_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_invitations
    ADD CONSTRAINT team_invitations_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id) ON DELETE CASCADE;


--
-- Name: team_machine_grants team_machine_grants_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_machine_grants
    ADD CONSTRAINT team_machine_grants_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: team_machine_grants team_machine_grants_team_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_machine_grants
    ADD CONSTRAINT team_machine_grants_team_id_account_id_fkey FOREIGN KEY (team_id, account_id) REFERENCES paperboat.team_members(team_id, account_id) ON DELETE CASCADE;


--
-- Name: team_machine_grants team_machine_grants_team_id_resource_kind_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_machine_grants
    ADD CONSTRAINT team_machine_grants_team_id_resource_kind_machine_id_fkey FOREIGN KEY (team_id, resource_kind, machine_id) REFERENCES paperboat.team_resource_bindings(team_id, resource_kind, resource_id) ON DELETE CASCADE;


--
-- Name: team_members team_members_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_members
    ADD CONSTRAINT team_members_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id);


--
-- Name: team_members team_members_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_members
    ADD CONSTRAINT team_members_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id) ON DELETE CASCADE;


--
-- Name: team_network_preferences team_network_preferences_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_network_preferences
    ADD CONSTRAINT team_network_preferences_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id) ON DELETE CASCADE;


--
-- Name: team_operations team_operations_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_operations
    ADD CONSTRAINT team_operations_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: team_receipt_smtp team_receipt_smtp_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_receipt_smtp
    ADD CONSTRAINT team_receipt_smtp_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id) ON DELETE CASCADE;


--
-- Name: team_resource_bindings team_resource_bindings_owner_account_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_resource_bindings
    ADD CONSTRAINT team_resource_bindings_owner_account_fkey FOREIGN KEY (owner_account) REFERENCES paperboat.users(id);


--
-- Name: team_resource_bindings team_resource_bindings_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_resource_bindings
    ADD CONSTRAINT team_resource_bindings_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id) ON DELETE CASCADE;


--
-- Name: team_resource_grants team_resource_grants_team_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_resource_grants
    ADD CONSTRAINT team_resource_grants_team_id_account_id_fkey FOREIGN KEY (team_id, account_id) REFERENCES paperboat.team_members(team_id, account_id) ON DELETE CASCADE;


--
-- Name: team_resource_grants team_resource_grants_team_id_resource_kind_resource_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_resource_grants
    ADD CONSTRAINT team_resource_grants_team_id_resource_kind_resource_id_fkey FOREIGN KEY (team_id, resource_kind, resource_id) REFERENCES paperboat.team_resource_bindings(team_id, resource_kind, resource_id) ON DELETE CASCADE;


--
-- Name: team_terminal_session_grants team_terminal_session_grants_team_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_terminal_session_grants
    ADD CONSTRAINT team_terminal_session_grants_team_id_account_id_fkey FOREIGN KEY (team_id, account_id) REFERENCES paperboat.team_members(team_id, account_id) ON DELETE CASCADE;


--
-- Name: team_terminal_session_grants team_terminal_session_grants_team_id_resource_kind_termina_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.team_terminal_session_grants
    ADD CONSTRAINT team_terminal_session_grants_team_id_resource_kind_termina_fkey FOREIGN KEY (team_id, resource_kind, terminal_session_id) REFERENCES paperboat.team_resource_bindings(team_id, resource_kind, resource_id) ON DELETE CASCADE;


--
-- Name: teams teams_owner_account_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.teams
    ADD CONSTRAINT teams_owner_account_fkey FOREIGN KEY (owner_account) REFERENCES paperboat.users(id);


--
-- Name: teams teams_suspended_by_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.teams
    ADD CONSTRAINT teams_suspended_by_fkey FOREIGN KEY (suspended_by) REFERENCES paperboat.users(id);


--
-- Name: tunnel_certificate_edge_distributions tunnel_certificate_edge_distributions_certificate_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_certificate_edge_distributions
    ADD CONSTRAINT tunnel_certificate_edge_distributions_certificate_id_fkey FOREIGN KEY (certificate_id) REFERENCES paperboat.tunnel_certificate_records(id) ON DELETE CASCADE;


--
-- Name: tunnel_certificate_edge_distributions tunnel_certificate_edge_distributions_edge_node_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_certificate_edge_distributions
    ADD CONSTRAINT tunnel_certificate_edge_distributions_edge_node_id_fkey FOREIGN KEY (edge_node_id) REFERENCES paperboat.control_tunnel_nodes(id) ON DELETE RESTRICT;


--
-- Name: tunnel_certificate_records tunnel_certificate_records_preview_lease_fk; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_certificate_records
    ADD CONSTRAINT tunnel_certificate_records_preview_lease_fk FOREIGN KEY (preview_id, account_id) REFERENCES paperboat.preview_leases(id, account_id) ON DELETE CASCADE;


--
-- Name: tunnel_certificate_records tunnel_certificate_records_user_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_certificate_records
    ADD CONSTRAINT tunnel_certificate_records_user_account_id_fkey FOREIGN KEY (user_account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: tunnel_config_generations tunnel_config_generations_tunnel_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_config_generations
    ADD CONSTRAINT tunnel_config_generations_tunnel_id_fkey FOREIGN KEY (tunnel_id) REFERENCES paperboat.tunnels(id) ON DELETE CASCADE;


--
-- Name: tunnel_config_generations tunnel_config_generations_tunnel_id_previous_generation_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_config_generations
    ADD CONSTRAINT tunnel_config_generations_tunnel_id_previous_generation_fkey FOREIGN KEY (tunnel_id, previous_generation) REFERENCES paperboat.tunnel_config_generations(tunnel_id, generation) ON DELETE RESTRICT;


--
-- Name: tunnel_connector_activations tunnel_connector_activations_connector_id_tunnel_id_host_i_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_activations
    ADD CONSTRAINT tunnel_connector_activations_connector_id_tunnel_id_host_i_fkey FOREIGN KEY (connector_id, tunnel_id, host_id) REFERENCES paperboat.tunnel_connectors(id, tunnel_id, host_id) ON DELETE CASCADE;


--
-- Name: tunnel_connector_activations tunnel_connector_activations_operation_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_activations
    ADD CONSTRAINT tunnel_connector_activations_operation_id_account_id_fkey FOREIGN KEY (operation_id, account_id) REFERENCES paperboat.operations(id, account_id) ON DELETE RESTRICT;


--
-- Name: tunnel_connector_credential_generations tunnel_connector_credential_generat_connector_id_tunnel_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_credential_generations
    ADD CONSTRAINT tunnel_connector_credential_generat_connector_id_tunnel_id_fkey FOREIGN KEY (connector_id, tunnel_id) REFERENCES paperboat.tunnel_connectors(id, tunnel_id) ON DELETE CASCADE;


--
-- Name: tunnel_connector_credential_generations tunnel_connector_credential_generation_source_operation_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_credential_generations
    ADD CONSTRAINT tunnel_connector_credential_generation_source_operation_id_fkey FOREIGN KEY (source_operation_id) REFERENCES paperboat.operations(id) ON DELETE SET NULL;


--
-- Name: tunnel_connector_credential_generations tunnel_connector_credential_generations_connector_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_credential_generations
    ADD CONSTRAINT tunnel_connector_credential_generations_connector_id_fkey FOREIGN KEY (connector_id) REFERENCES paperboat.tunnel_connectors(id) ON DELETE CASCADE;


--
-- Name: tunnel_connector_credential_generations tunnel_connector_credential_generations_tunnel_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_credential_generations
    ADD CONSTRAINT tunnel_connector_credential_generations_tunnel_id_fkey FOREIGN KEY (tunnel_id) REFERENCES paperboat.tunnels(id) ON DELETE CASCADE;


--
-- Name: tunnel_connector_enrollments tunnel_connector_enrollments_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_enrollments
    ADD CONSTRAINT tunnel_connector_enrollments_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: tunnel_connector_enrollments tunnel_connector_enrollments_connector_id_tunnel_id_host_i_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_enrollments
    ADD CONSTRAINT tunnel_connector_enrollments_connector_id_tunnel_id_host_i_fkey FOREIGN KEY (connector_id, tunnel_id, host_id) REFERENCES paperboat.tunnel_connectors(id, tunnel_id, host_id) ON DELETE RESTRICT;


--
-- Name: tunnel_connector_enrollments tunnel_connector_enrollments_operation_account_fk; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_enrollments
    ADD CONSTRAINT tunnel_connector_enrollments_operation_account_fk FOREIGN KEY (operation_id, account_id) REFERENCES paperboat.operations(id, account_id) ON DELETE RESTRICT;


--
-- Name: tunnel_connector_enrollments tunnel_connector_enrollments_operation_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_enrollments
    ADD CONSTRAINT tunnel_connector_enrollments_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES paperboat.operations(id) ON DELETE RESTRICT;


--
-- Name: tunnel_connector_enrollments tunnel_connector_enrollments_tunnel_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_enrollments
    ADD CONSTRAINT tunnel_connector_enrollments_tunnel_id_account_id_fkey FOREIGN KEY (tunnel_id, account_id) REFERENCES paperboat.tunnels(id, account_id) ON DELETE CASCADE;


--
-- Name: tunnel_connector_enrollments tunnel_connector_enrollments_tunnel_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_enrollments
    ADD CONSTRAINT tunnel_connector_enrollments_tunnel_id_fkey FOREIGN KEY (tunnel_id) REFERENCES paperboat.tunnels(id) ON DELETE CASCADE;


--
-- Name: tunnel_connector_rotation_targets tunnel_connector_rotation_tar_connector_id_tunnel_id_host__fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_rotation_targets
    ADD CONSTRAINT tunnel_connector_rotation_tar_connector_id_tunnel_id_host__fkey FOREIGN KEY (connector_id, tunnel_id, host_id) REFERENCES paperboat.tunnel_connectors(id, tunnel_id, host_id) ON DELETE CASCADE;


--
-- Name: tunnel_connector_rotation_targets tunnel_connector_rotation_targets_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_rotation_targets
    ADD CONSTRAINT tunnel_connector_rotation_targets_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: tunnel_connector_rotation_targets tunnel_connector_rotation_targets_operation_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_rotation_targets
    ADD CONSTRAINT tunnel_connector_rotation_targets_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES paperboat.operations(id) ON DELETE CASCADE;


--
-- Name: tunnel_connector_rotation_targets tunnel_connector_rotation_targets_tunnel_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_rotation_targets
    ADD CONSTRAINT tunnel_connector_rotation_targets_tunnel_id_account_id_fkey FOREIGN KEY (tunnel_id, account_id) REFERENCES paperboat.tunnels(id, account_id) ON DELETE CASCADE;


--
-- Name: tunnel_connector_sessions tunnel_connector_sessions_connector_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connector_sessions
    ADD CONSTRAINT tunnel_connector_sessions_connector_id_fkey FOREIGN KEY (connector_id) REFERENCES paperboat.tunnel_connectors(id) ON DELETE CASCADE;


--
-- Name: tunnel_connectors tunnel_connectors_tunnel_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_connectors
    ADD CONSTRAINT tunnel_connectors_tunnel_id_fkey FOREIGN KEY (tunnel_id) REFERENCES paperboat.tunnels(id) ON DELETE CASCADE;


--
-- Name: tunnel_domains tunnel_domains_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_domains
    ADD CONSTRAINT tunnel_domains_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: tunnel_domains tunnel_domains_route_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_domains
    ADD CONSTRAINT tunnel_domains_route_id_fkey FOREIGN KEY (route_id) REFERENCES paperboat.tunnel_routes(id) ON DELETE CASCADE;


--
-- Name: tunnel_domains tunnel_domains_route_tunnel_fk; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_domains
    ADD CONSTRAINT tunnel_domains_route_tunnel_fk FOREIGN KEY (route_id, tunnel_id) REFERENCES paperboat.tunnel_routes(id, tunnel_id) ON DELETE CASCADE;


--
-- Name: tunnel_domains tunnel_domains_tunnel_account_fk; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_domains
    ADD CONSTRAINT tunnel_domains_tunnel_account_fk FOREIGN KEY (tunnel_id, account_id) REFERENCES paperboat.tunnels(id, account_id) ON DELETE CASCADE;


--
-- Name: tunnel_domains tunnel_domains_tunnel_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_domains
    ADD CONSTRAINT tunnel_domains_tunnel_id_fkey FOREIGN KEY (tunnel_id) REFERENCES paperboat.tunnels(id) ON DELETE CASCADE;


--
-- Name: tunnel_edge_route_assignments tunnel_edge_route_assignments_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_edge_route_assignments
    ADD CONSTRAINT tunnel_edge_route_assignments_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: tunnel_edge_route_assignments tunnel_edge_route_assignments_connector_id_tunnel_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_edge_route_assignments
    ADD CONSTRAINT tunnel_edge_route_assignments_connector_id_tunnel_id_fkey FOREIGN KEY (connector_id, tunnel_id) REFERENCES paperboat.tunnel_connectors(id, tunnel_id) ON DELETE CASCADE;


--
-- Name: tunnel_edge_route_assignments tunnel_edge_route_assignments_connector_id_tunnel_id_host__fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_edge_route_assignments
    ADD CONSTRAINT tunnel_edge_route_assignments_connector_id_tunnel_id_host__fkey FOREIGN KEY (connector_id, tunnel_id, host_id) REFERENCES paperboat.tunnel_connectors(id, tunnel_id, host_id) ON DELETE CASCADE;


--
-- Name: tunnel_edge_route_assignments tunnel_edge_route_assignments_connector_session_id_connect_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_edge_route_assignments
    ADD CONSTRAINT tunnel_edge_route_assignments_connector_session_id_connect_fkey FOREIGN KEY (connector_session_id, connector_id) REFERENCES paperboat.tunnel_connector_sessions(id, connector_id) ON DELETE CASCADE;


--
-- Name: tunnel_edge_route_assignments tunnel_edge_route_assignments_edge_node_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_edge_route_assignments
    ADD CONSTRAINT tunnel_edge_route_assignments_edge_node_id_fkey FOREIGN KEY (edge_node_id) REFERENCES paperboat.control_tunnel_nodes(id) ON DELETE RESTRICT;


--
-- Name: tunnel_edge_route_assignments tunnel_edge_route_assignments_route_id_tunnel_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_edge_route_assignments
    ADD CONSTRAINT tunnel_edge_route_assignments_route_id_tunnel_id_fkey FOREIGN KEY (route_id, tunnel_id) REFERENCES paperboat.tunnel_routes(id, tunnel_id) ON DELETE CASCADE;


--
-- Name: tunnel_edge_route_assignments tunnel_edge_route_assignments_tunnel_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_edge_route_assignments
    ADD CONSTRAINT tunnel_edge_route_assignments_tunnel_id_account_id_fkey FOREIGN KEY (tunnel_id, account_id) REFERENCES paperboat.tunnels(id, account_id) ON DELETE CASCADE;


--
-- Name: tunnel_edge_route_assignments tunnel_edge_route_assignments_tunnel_id_config_generation_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_edge_route_assignments
    ADD CONSTRAINT tunnel_edge_route_assignments_tunnel_id_config_generation_fkey FOREIGN KEY (tunnel_id, config_generation) REFERENCES paperboat.tunnel_config_generations(tunnel_id, generation) ON DELETE RESTRICT;


--
-- Name: tunnel_log_entries tunnel_log_entries_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_log_entries
    ADD CONSTRAINT tunnel_log_entries_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: tunnel_log_entries tunnel_log_entries_connector_id_tunnel_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_log_entries
    ADD CONSTRAINT tunnel_log_entries_connector_id_tunnel_id_fkey FOREIGN KEY (connector_id, tunnel_id) REFERENCES paperboat.tunnel_connectors(id, tunnel_id) ON DELETE CASCADE;


--
-- Name: tunnel_log_entries tunnel_log_entries_preview_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_log_entries
    ADD CONSTRAINT tunnel_log_entries_preview_id_account_id_fkey FOREIGN KEY (preview_id, account_id) REFERENCES paperboat.preview_leases(id, account_id) ON DELETE CASCADE;


--
-- Name: tunnel_log_entries tunnel_log_entries_preview_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_log_entries
    ADD CONSTRAINT tunnel_log_entries_preview_id_fkey FOREIGN KEY (preview_id) REFERENCES paperboat.preview_leases(id) ON DELETE CASCADE;


--
-- Name: tunnel_log_entries tunnel_log_entries_route_id_tunnel_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_log_entries
    ADD CONSTRAINT tunnel_log_entries_route_id_tunnel_id_fkey FOREIGN KEY (route_id, tunnel_id) REFERENCES paperboat.tunnel_routes(id, tunnel_id) ON DELETE CASCADE;


--
-- Name: tunnel_log_entries tunnel_log_entries_tunnel_id_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_log_entries
    ADD CONSTRAINT tunnel_log_entries_tunnel_id_account_id_fkey FOREIGN KEY (tunnel_id, account_id) REFERENCES paperboat.tunnels(id, account_id) ON DELETE CASCADE;


--
-- Name: tunnel_log_entries tunnel_log_entries_tunnel_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_log_entries
    ADD CONSTRAINT tunnel_log_entries_tunnel_id_fkey FOREIGN KEY (tunnel_id) REFERENCES paperboat.tunnels(id) ON DELETE CASCADE;


--
-- Name: tunnel_routes tunnel_routes_tunnel_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnel_routes
    ADD CONSTRAINT tunnel_routes_tunnel_id_fkey FOREIGN KEY (tunnel_id) REFERENCES paperboat.tunnels(id) ON DELETE CASCADE;


--
-- Name: tunnels tunnels_account_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.tunnels
    ADD CONSTRAINT tunnels_account_id_fkey FOREIGN KEY (account_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: usage_accounting_authorities usage_accounting_authorities_edge_node_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.usage_accounting_authorities
    ADD CONSTRAINT usage_accounting_authorities_edge_node_id_fkey FOREIGN KEY (edge_node_id) REFERENCES paperboat.control_tunnel_nodes(id) ON DELETE CASCADE;


--
-- Name: usage_accounting_authorities usage_accounting_authorities_workspace_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.usage_accounting_authorities
    ADD CONSTRAINT usage_accounting_authorities_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES paperboat.billing_workspaces(id);


--
-- Name: user_favorites user_favorites_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_favorites
    ADD CONSTRAINT user_favorites_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: user_identities user_identities_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_identities
    ADD CONSTRAINT user_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id);


--
-- Name: user_machine_access_sessions user_machine_access_sessions_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_access_sessions
    ADD CONSTRAINT user_machine_access_sessions_team_id_fkey FOREIGN KEY (team_id) REFERENCES paperboat.teams(team_id);


--
-- Name: user_machine_access_sessions user_machine_access_sessions_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_access_sessions
    ADD CONSTRAINT user_machine_access_sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: user_machine_access_sessions user_machine_access_sessions_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_access_sessions
    ADD CONSTRAINT user_machine_access_sessions_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: user_machine_availability_operations user_machine_availability_operations_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_availability_operations
    ADD CONSTRAINT user_machine_availability_operations_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: user_machine_availability_operations user_machine_availability_operations_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_availability_operations
    ADD CONSTRAINT user_machine_availability_operations_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: user_machine_bandwidth_periods user_machine_bandwidth_periods_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_bandwidth_periods
    ADD CONSTRAINT user_machine_bandwidth_periods_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: user_machine_bandwidth_topups user_machine_bandwidth_topups_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_bandwidth_topups
    ADD CONSTRAINT user_machine_bandwidth_topups_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: user_machine_capability_operations user_machine_capability_operations_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_capability_operations
    ADD CONSTRAINT user_machine_capability_operations_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: user_machine_capability_operations user_machine_capability_operations_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_capability_operations
    ADD CONSTRAINT user_machine_capability_operations_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: user_machine_enrollments user_machine_enrollments_pairing_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_enrollments
    ADD CONSTRAINT user_machine_enrollments_pairing_id_fkey FOREIGN KEY (pairing_id) REFERENCES paperboat.user_machine_pairings(id) ON DELETE SET NULL;


--
-- Name: user_machine_enrollments user_machine_enrollments_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_enrollments
    ADD CONSTRAINT user_machine_enrollments_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: user_machine_enrollments user_machine_enrollments_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_enrollments
    ADD CONSTRAINT user_machine_enrollments_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE SET NULL;


--
-- Name: user_machine_entitlements user_machine_entitlements_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_entitlements
    ADD CONSTRAINT user_machine_entitlements_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: user_machine_maintenance_approvals user_machine_maintenance_approvals_decided_by_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_maintenance_approvals
    ADD CONSTRAINT user_machine_maintenance_approvals_decided_by_user_id_fkey FOREIGN KEY (decided_by_user_id) REFERENCES paperboat.users(id) ON DELETE SET NULL;


--
-- Name: user_machine_maintenance_approvals user_machine_maintenance_approvals_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_maintenance_approvals
    ADD CONSTRAINT user_machine_maintenance_approvals_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: user_machine_maintenance_approvals user_machine_maintenance_approvals_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_maintenance_approvals
    ADD CONSTRAINT user_machine_maintenance_approvals_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: user_machine_pairings user_machine_pairings_approved_by_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_pairings
    ADD CONSTRAINT user_machine_pairings_approved_by_user_id_fkey FOREIGN KEY (approved_by_user_id) REFERENCES paperboat.users(id) ON DELETE SET NULL;


--
-- Name: user_machine_pairings user_machine_pairings_authenticated_setup_cli_session_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_pairings
    ADD CONSTRAINT user_machine_pairings_authenticated_setup_cli_session_id_fkey FOREIGN KEY (authenticated_setup_cli_session_id) REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE;


--
-- Name: user_machine_pairings user_machine_pairings_authenticated_setup_helper_enrollmen_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_pairings
    ADD CONSTRAINT user_machine_pairings_authenticated_setup_helper_enrollmen_fkey FOREIGN KEY (authenticated_setup_helper_enrollment_id) REFERENCES paperboat.control_helper_enrollments(id) ON DELETE SET NULL;


--
-- Name: user_machine_pairings user_machine_pairings_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_pairings
    ADD CONSTRAINT user_machine_pairings_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE SET NULL;


--
-- Name: user_machine_terminal_session_operations user_machine_terminal_session_operatio_terminal_session_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_terminal_session_operations
    ADD CONSTRAINT user_machine_terminal_session_operatio_terminal_session_id_fkey FOREIGN KEY (terminal_session_id) REFERENCES paperboat.user_machine_terminal_sessions(id) ON DELETE CASCADE;


--
-- Name: user_machine_terminal_session_operations user_machine_terminal_session_operations_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_terminal_session_operations
    ADD CONSTRAINT user_machine_terminal_session_operations_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: user_machine_terminal_sessions user_machine_terminal_session_transfer_destination_machine_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_terminal_sessions
    ADD CONSTRAINT user_machine_terminal_session_transfer_destination_machine_fkey FOREIGN KEY (transfer_destination_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE SET NULL;


--
-- Name: user_machine_terminal_sessions user_machine_terminal_sessions_owner_account_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_terminal_sessions
    ADD CONSTRAINT user_machine_terminal_sessions_owner_account_fkey FOREIGN KEY (owner_account) REFERENCES paperboat.users(id);


--
-- Name: user_machine_terminal_sessions user_machine_terminal_sessions_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_terminal_sessions
    ADD CONSTRAINT user_machine_terminal_sessions_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: user_machine_update_observations user_machine_update_observations_user_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machine_update_observations
    ADD CONSTRAINT user_machine_update_observations_user_machine_id_fkey FOREIGN KEY (user_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: user_machines user_machines_owner_team_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machines
    ADD CONSTRAINT user_machines_owner_team_id_fkey FOREIGN KEY (owner_team_id) REFERENCES paperboat.teams(team_id);


--
-- Name: user_machines user_machines_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_machines
    ADD CONSTRAINT user_machines_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- Name: user_transfer_destination_defaults user_transfer_destination_defaults_machine_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_transfer_destination_defaults
    ADD CONSTRAINT user_transfer_destination_defaults_machine_id_fkey FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;


--
-- Name: user_transfer_destination_defaults user_transfer_destination_defaults_user_id_fkey; Type: FK CONSTRAINT; Schema: paperboat; Owner: -
--

ALTER TABLE ONLY paperboat.user_transfer_destination_defaults
    ADD CONSTRAINT user_transfer_destination_defaults_user_id_fkey FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;


--
-- PostgreSQL database dump complete
--




-- Workload indexes retained in the supported initial schema.
-- Control revocation feeds read unexpired revoked sessions on every poll.
-- The previous indexes only cover active sessions or helper cleanup.
CREATE INDEX user_machine_access_sessions_revocation_expiry
    ON paperboat.user_machine_access_sessions (expires_at)
    WHERE revoked_at IS NOT NULL;

-- Session revocation and lifecycle joins need every token state. The existing
-- unique index only covers active tokens, so these paths scan the whole table.
CREATE INDEX cli_refresh_tokens_session
    ON paperboat.cli_refresh_tokens (cli_client_session_id);

-- The bounded due-operation poll orders pending work by creation time.
CREATE INDEX user_machine_terminal_session_operations_pending_created
    ON paperboat.user_machine_terminal_session_operations (created_at)
    WHERE state = 'pending';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'the baseline migration is forward-only';
END $$;
-- +goose StatementEnd
