-- +goose Up
-- The pending-work lookup and FK cascade both need every session state.
CREATE INDEX user_machine_access_sessions_machine_cleanup ON paperboat.user_machine_access_sessions(user_machine_id);
CREATE INDEX user_machines_final_delete ON paperboat.user_machines
 ((coalesce(deleted_at,revoked_at,created_at)),id) WHERE deleted_at IS NOT NULL OR state='revoked';
-- History owns its machine identity; operational state belongs to the machine.
ALTER TABLE paperboat.team_inbox_requests
    ADD COLUMN source_machine_name text NOT NULL DEFAULT '',
    ADD COLUMN destination_machine_name text NOT NULL DEFAULT '';
UPDATE paperboat.team_inbox_requests r SET source_machine_name=m.alias
FROM paperboat.user_machines m WHERE m.id=r.source_machine_id;
UPDATE paperboat.team_inbox_requests r SET destination_machine_name=m.alias
FROM paperboat.user_machines m WHERE m.id=r.destination_machine_id;

ALTER TABLE paperboat.user_machine_bandwidth_periods
    ADD COLUMN user_id text,
    ADD COLUMN machine_name text NOT NULL DEFAULT '';
UPDATE paperboat.user_machine_bandwidth_periods p SET user_id=m.user_id,machine_name=m.alias
FROM paperboat.user_machines m WHERE m.id=p.user_machine_id;
ALTER TABLE paperboat.user_machine_bandwidth_periods ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE paperboat.user_machine_bandwidth_periods ADD CONSTRAINT user_machine_bandwidth_periods_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;
CREATE INDEX user_machine_bandwidth_periods_account_period
    ON paperboat.user_machine_bandwidth_periods(user_id,period_start,period_end);

-- Capture names once, including for inserts by accounting and inbox callers.
-- +goose StatementBegin
CREATE FUNCTION paperboat.snapshot_machine_history() RETURNS trigger
LANGUAGE plpgsql SET search_path TO paperboat,pg_catalog AS $$
BEGIN
 IF TG_TABLE_NAME='team_inbox_requests' THEN
  SELECT alias INTO STRICT NEW.source_machine_name FROM user_machines WHERE id=NEW.source_machine_id;
  SELECT alias INTO STRICT NEW.destination_machine_name FROM user_machines WHERE id=NEW.destination_machine_id;
 ELSE
  SELECT user_id,alias INTO STRICT NEW.user_id,NEW.machine_name FROM user_machines WHERE id=NEW.user_machine_id;
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER team_inbox_requests_machine_snapshot BEFORE INSERT ON paperboat.team_inbox_requests
    FOR EACH ROW EXECUTE FUNCTION paperboat.snapshot_machine_history();
CREATE TRIGGER user_machine_bandwidth_periods_machine_snapshot BEFORE INSERT ON paperboat.user_machine_bandwidth_periods
    FOR EACH ROW EXECUTE FUNCTION paperboat.snapshot_machine_history();

-- Public hostname reservations are history: a removed URL cannot be reassigned.
-- Every existing machine FK is classified here, including composite FKs. Access
-- sessions retain their FK: the worker must deliver every revocation first.
-- +goose StatementBegin
DO $$
DECLARE fk record;
BEGIN
 FOR fk IN SELECT c.conrelid::regclass AS tbl,c.conname,pg_get_constraintdef(c.oid) AS definition
  FROM pg_constraint c WHERE c.contype='f' AND c.confrelid='paperboat.user_machines'::regclass
    AND c.conrelid<>'paperboat.user_machine_access_sessions'::regclass
 LOOP
  IF fk.tbl IN ('paperboat.team_inbox_requests'::regclass,'paperboat.user_machine_bandwidth_periods'::regclass,'paperboat.managed_hostnames'::regclass) THEN
   EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I',fk.tbl,fk.conname);
  ELSIF fk.definition NOT LIKE '%ON DELETE CASCADE%' THEN
   EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I',fk.tbl,fk.conname);
   EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I %s',fk.tbl,fk.conname,
    regexp_replace(fk.definition,' ON DELETE (SET NULL|RESTRICT|NO ACTION|SET DEFAULT)','') || ' ON DELETE CASCADE');
  END IF;
 END LOOP;
END $$;
-- +goose StatementEnd

-- Credentials and their issued certificates are operational too. Restrictive
-- child FKs would otherwise block the machine cascade.
ALTER TABLE paperboat.account_e2ee_keys DROP CONSTRAINT account_e2ee_keys_cli_client_session_id_fkey;
ALTER TABLE paperboat.account_e2ee_keys ADD CONSTRAINT account_e2ee_keys_cli_client_session_id_fkey
    FOREIGN KEY (cli_client_session_id) REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE;
ALTER TABLE paperboat.peer_endpoint_certificate_operations DROP CONSTRAINT peer_endpoint_certificate_operatio_certificate_fingerprint_fkey;
ALTER TABLE paperboat.peer_endpoint_certificate_operations ADD CONSTRAINT peer_endpoint_certificate_operatio_certificate_fingerprint_fkey
    FOREIGN KEY (certificate_fingerprint) REFERENCES paperboat.peer_endpoint_certificates(fingerprint) ON DELETE CASCADE;
ALTER TABLE paperboat.peer_endpoint_certificate_revocations DROP CONSTRAINT peer_endpoint_certificate_revocati_certificate_fingerprint_fkey;
ALTER TABLE paperboat.peer_endpoint_certificate_revocations ADD CONSTRAINT peer_endpoint_certificate_revocati_certificate_fingerprint_fkey
    FOREIGN KEY (certificate_fingerprint) REFERENCES paperboat.peer_endpoint_certificates(fingerprint) ON DELETE CASCADE;
ALTER TABLE paperboat.peer_network_identities DROP CONSTRAINT peer_network_identities_quic_certificate_fingerprint_fkey;
ALTER TABLE paperboat.peer_network_identities ADD CONSTRAINT peer_network_identities_quic_certificate_fingerprint_fkey
    FOREIGN KEY (quic_certificate_fingerprint) REFERENCES paperboat.peer_endpoint_certificates(fingerprint) ON DELETE CASCADE;
ALTER TABLE paperboat.machine_control_sessions DROP CONSTRAINT machine_control_sessions_operation_id_machine_id_installat_fkey;
ALTER TABLE paperboat.machine_control_sessions ADD CONSTRAINT machine_control_sessions_operation_id_machine_id_installat_fkey
    FOREIGN KEY (operation_id,machine_id,installation_generation)
    REFERENCES paperboat.machine_control_renewals(operation_id,machine_id,installation_generation) ON DELETE CASCADE;
ALTER TABLE paperboat.machine_ssh_host_keys DROP CONSTRAINT machine_ssh_host_keys_fingerprint_user_machine_id_fkey;
ALTER TABLE paperboat.machine_ssh_host_keys ADD CONSTRAINT machine_ssh_host_keys_fingerprint_user_machine_id_fkey
    FOREIGN KEY (fingerprint,user_machine_id) REFERENCES paperboat.machine_ssh_host_key_owners(fingerprint,user_machine_id) ON DELETE CASCADE;
ALTER TABLE paperboat.control_config_sync_migration_reviews DROP CONSTRAINT control_config_sync_migration_reviews_environment_id_fkey;
ALTER TABLE paperboat.control_config_sync_migration_reviews ADD CONSTRAINT control_config_sync_migration_reviews_environment_id_fkey
    FOREIGN KEY (environment_id) REFERENCES paperboat.control_environments(id) ON DELETE CASCADE;

-- Accounting must outlive environment cleanup as well as machine cleanup.
ALTER TABLE paperboat.control_usage_counters DROP CONSTRAINT control_usage_counters_environment_id_fkey;
-- Repository fencing counters belong to the repository and must survive, but
-- their current machine lease and ephemeral operation responses do not.
-- +goose StatementBegin
CREATE FUNCTION paperboat.delete_machine_environment_operations() RETURNS trigger
LANGUAGE plpgsql SET search_path TO paperboat,pg_catalog AS $$
BEGIN
 UPDATE control_config_repository_lease_authority SET
  lease_id=NULL,assignment_id=NULL,environment_id=NULL,machine_id=NULL,
  installation_generation=NULL,base_remote_revision=NULL,operation_id=NULL,
  acquired_at=NULL,expires_at=NULL,revoked_at=NULL,version=version+1,updated_at=now()
 WHERE environment_id=OLD.id;
 DELETE FROM control_config_repository_lease_operations WHERE environment_id=OLD.id;
 DELETE FROM control_config_repository_access_operations WHERE environment_id=OLD.id;
 DELETE FROM control_config_credentials WHERE environment_id=OLD.id;
 DELETE FROM helper_identity_renewals WHERE environment_id=OLD.id;
 DELETE FROM lazy_access_policies WHERE environment_id=OLD.id;
 DELETE FROM user_machine_update_observations WHERE environment_id=OLD.id;
 RETURN OLD;
END $$;
-- +goose StatementEnd
CREATE TRIGGER control_environments_delete_operations BEFORE DELETE ON paperboat.control_environments
    FOR EACH ROW EXECUTE FUNCTION paperboat.delete_machine_environment_operations();
-- Revoked is a removal state, including rows written before this migration.
-- Normalize it once so every existing deleted_at fence also hides revoked machines.
-- +goose StatementBegin
CREATE FUNCTION paperboat.tombstone_revoked_machine() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 NEW.deleted_at=coalesce(NEW.deleted_at,now());
 NEW.revoked_at=coalesce(NEW.revoked_at,now());
 NEW.online=false;
 NEW.seat_state='released';
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER machine_revoked_tombstone BEFORE INSERT OR UPDATE OF state ON paperboat.user_machines
 FOR EACH ROW WHEN (NEW.state='revoked') EXECUTE FUNCTION paperboat.tombstone_revoked_machine();

-- All revoke producers share the same environment fence and durable session
-- revocations, as the existing machine/peer and team-entry triggers do.
-- +goose StatementBegin
CREATE FUNCTION paperboat.revoke_machine_environment() RETURNS trigger
LANGUAGE plpgsql SET search_path TO paperboat,pg_catalog AS $$
BEGIN
 UPDATE control_environments SET desired_state='revoked',revoked_at=coalesce(revoked_at,now()),
  desired_version=desired_version+1,updated_at=now()
 WHERE id=NEW.environment_id AND desired_state<>'revoked';
 UPDATE control_helpers SET state='revoked',revoked_at=now(),updated_at=now()
 WHERE environment_id=NEW.environment_id AND state IN ('pending','active') AND revoked_at IS NULL;
 UPDATE control_helper_enrollments SET state='revoked',revoked_at=now()
 WHERE environment_id=NEW.environment_id AND state='pending' AND revoked_at IS NULL;
 UPDATE control_connector_generations SET state='revoked',revoked_at=now(),version=version+1,updated_at=now()
 WHERE environment_id=NEW.environment_id AND state IN ('pending','admitted','draining') AND revoked_at IS NULL;
 UPDATE control_routes SET desired_state=CASE WHEN applied_node_id IS NULL THEN 'detached' ELSE 'detaching' END,
  desired_revision=desired_revision+1,version=version+1,updated_at=now()
 WHERE environment_id=NEW.environment_id AND desired_state NOT IN ('detaching','detached');
 PERFORM revoke_config_sync_for_environment(NEW.environment_id);
 UPDATE cli_client_sessions SET state='revoked',revoked_at=coalesce(revoked_at,now()),
  revocation_reason=coalesce(revocation_reason,'machine_removed'),version=version+1
 WHERE user_machine_id=NEW.id AND state='active';
 UPDATE cli_access_tokens SET revoked_at=coalesce(revoked_at,now())
 WHERE cli_client_session_id IN (SELECT id FROM cli_client_sessions WHERE user_machine_id=NEW.id);
 UPDATE cli_refresh_tokens SET state='revoked',revoked_at=coalesce(revoked_at,now())
 WHERE cli_client_session_id IN (SELECT id FROM cli_client_sessions WHERE user_machine_id=NEW.id) AND state<>'revoked';
 UPDATE user_machine_access_sessions SET state='revoked',revoked_at=coalesce(revoked_at,now()),
  revocation_reason=coalesce(revocation_reason,'user_machine_revoked'),updated_at=now()
 WHERE user_machine_id=NEW.id AND state='active';
 INSERT INTO audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,idempotency_key,metadata)
 VALUES('event_'||gen_random_uuid()::text,NEW.user_id,'system','user_machine.revoked','user_machine',NEW.id,
  'user_machine.revoked:'||NEW.id,jsonb_build_object('alias',NEW.alias))
 ON CONFLICT(event_type,resource_type,resource_id,idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER machine_revoked_environment AFTER INSERT OR UPDATE OF state ON paperboat.user_machines
 FOR EACH ROW WHEN (NEW.state='revoked') EXECUTE FUNCTION paperboat.revoke_machine_environment();
UPDATE paperboat.user_machines SET state=state,deleted_at=coalesce(deleted_at,now()),online=false,seat_state='released'
 WHERE state='revoked';
ALTER TABLE paperboat.user_machines ADD CONSTRAINT user_machines_revoked_tombstone
 CHECK(state<>'revoked' OR deleted_at IS NOT NULL);
DELETE FROM paperboat.control_environments e WHERE NOT EXISTS (
 SELECT 1 FROM paperboat.user_machines m WHERE m.environment_id=e.id
  AND m.deleted_at IS NULL AND m.state NOT IN ('revoked','deleted'));

-- Publications and polymorphic favorites previously had no machine FK.
DELETE FROM paperboat.preview_leases p WHERE NOT EXISTS (SELECT 1 FROM paperboat.user_machines m WHERE m.id=p.owner_machine_id);
DELETE FROM paperboat.tunnels t WHERE NOT EXISTS (SELECT 1 FROM paperboat.user_machines m WHERE m.id=t.created_by_host_id);
ALTER TABLE paperboat.preview_leases ADD CONSTRAINT preview_leases_owner_machine_id_fkey
    FOREIGN KEY (owner_machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;
ALTER TABLE paperboat.tunnels ADD CONSTRAINT tunnels_created_by_host_id_fkey
    FOREIGN KEY (created_by_host_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;
-- +goose StatementBegin
CREATE FUNCTION paperboat.delete_machine_operational_references() RETURNS trigger
LANGUAGE plpgsql SET search_path TO paperboat,pg_catalog AS $$
BEGIN
 DELETE FROM user_favorites WHERE (kind='machine' AND resource_id=OLD.id)
  OR (kind='session' AND resource_id IN (SELECT id FROM user_machine_terminal_sessions WHERE user_machine_id=OLD.id))
  OR (kind='preview' AND resource_id IN (SELECT id FROM preview_leases WHERE owner_machine_id=OLD.id));
 -- Endpoint certificates have independent signers (migration 11), so they
 -- belong to the enrolled endpoint, never to its ENV signing key.
 DELETE FROM peer_endpoint_enrollment_requests WHERE endpoint_id=OLD.id
  OR endpoint_id IN (SELECT id FROM cli_client_sessions WHERE user_machine_id=OLD.id);
 DELETE FROM peer_endpoint_certificates WHERE endpoint_id=OLD.id
  OR endpoint_id IN (SELECT id FROM cli_client_sessions WHERE user_machine_id=OLD.id);
 DELETE FROM control_environments WHERE id=OLD.environment_id;
 DELETE FROM environment_vault_personal_rotation_scopes WHERE machine_id=OLD.id;
 DELETE FROM environment_vault_scopes WHERE machine_id=OLD.id;
 DELETE FROM control_config_repository_access_operations WHERE machine_id=OLD.id;
 RETURN OLD;
END $$;
-- +goose StatementEnd
CREATE TRIGGER user_machines_delete_favorites BEFORE DELETE ON paperboat.user_machines
    FOR EACH ROW EXECUTE FUNCTION paperboat.delete_machine_operational_references();

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'machine deletion and history snapshots are forward-only'; END $$;
-- +goose StatementEnd
