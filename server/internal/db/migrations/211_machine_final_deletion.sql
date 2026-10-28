-- +goose Up
-- The pending-work lookup and FK cascade both need every session state.
CREATE INDEX user_machine_access_sessions_machine_cleanup ON paperboat.user_machine_access_sessions(user_machine_id);
CREATE INDEX user_machines_final_delete ON paperboat.user_machines(deleted_at,id) WHERE deleted_at IS NOT NULL;
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
  IF fk.tbl IN ('paperboat.team_inbox_requests'::regclass,'paperboat.user_machine_bandwidth_periods'::regclass) THEN
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

-- Migration 43 required every existing control scope to have an owning machine;
-- both current creation paths also create machine scopes. Preserve that origin
-- explicitly: a missing machine is never evidence of a standalone environment.
ALTER TABLE paperboat.control_environments ADD COLUMN machine_id text;
UPDATE paperboat.control_environments e SET machine_id=coalesce(
    (SELECT m.id FROM paperboat.user_machines m WHERE m.environment_id=e.id),e.id);
-- Accounting must outlive environment cleanup as well as machine cleanup.
ALTER TABLE paperboat.control_usage_counters DROP CONSTRAINT control_usage_counters_environment_id_fkey;
DELETE FROM paperboat.control_environments e WHERE NOT EXISTS (
    SELECT 1 FROM paperboat.user_machines m WHERE m.id=e.machine_id AND m.deleted_at IS NULL);
ALTER TABLE paperboat.control_environments ADD CONSTRAINT control_environments_machine_id_fkey
    FOREIGN KEY (machine_id) REFERENCES paperboat.user_machines(id) ON DELETE CASCADE;
CREATE UNIQUE INDEX control_environments_machine ON paperboat.control_environments(machine_id);

-- Bind either insertion order. Only environments never bound to a machine have
-- a NULL machine_id (standalone); hard deletion cascades instead of clearing it.
-- +goose StatementBegin
CREATE FUNCTION paperboat.bind_machine_environment() RETURNS trigger
LANGUAGE plpgsql SET search_path TO paperboat,pg_catalog AS $$
BEGIN
 IF TG_TABLE_NAME='control_environments' THEN
  IF NEW.machine_id IS NULL THEN
   SELECT id INTO NEW.machine_id FROM user_machines WHERE environment_id=NEW.id;
  END IF;
 ELSE
  UPDATE control_environments SET machine_id=NEW.id WHERE id=NEW.environment_id;
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER control_environments_machine BEFORE INSERT ON paperboat.control_environments
    FOR EACH ROW EXECUTE FUNCTION paperboat.bind_machine_environment();
CREATE TRIGGER user_machines_environment AFTER INSERT OR UPDATE OF environment_id ON paperboat.user_machines
    FOR EACH ROW EXECUTE FUNCTION paperboat.bind_machine_environment();

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
