-- +goose Up
-- The control scope is subordinate to the machine. Pause admission and drain
-- carriers, incomplete installations, outstanding usage deliveries, and
-- externally issued repository grants before this atomic cutover. Installed
-- machine keys and installation generations remain unchanged. Current clients
-- perform authenticated same-key setup to obtain fresh scope-bound credentials.
-- +goose StatementBegin
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM paperboat.control_environments e
            LEFT JOIN paperboat.user_machines m ON m.environment_id=e.id
            WHERE m.id IS NULL) THEN
  RAISE EXCEPTION 'machine scope cutover requires every control environment to have an owning machine';
 END IF;
 IF EXISTS (SELECT 1 FROM paperboat.user_machines m
            JOIN paperboat.control_environments e ON e.id=m.id
            WHERE m.environment_id<>m.id) THEN
  RAISE EXCEPTION 'machine scope cutover found a conflicting control scope identity';
 END IF;
 IF EXISTS (SELECT 1 FROM paperboat.runtime_carrier_attachments a
            JOIN paperboat.user_machines m ON m.id=a.machine_id
            WHERE m.environment_id<>m.id AND a.active AND a.expires_at>now())
 OR EXISTS (SELECT 1 FROM paperboat.user_machine_access_sessions a
            JOIN paperboat.user_machines m ON m.id=a.user_machine_id
            WHERE m.environment_id<>m.id AND a.state='active' AND a.expires_at>now()) THEN
  RAISE EXCEPTION 'machine scope cutover requires drained runtime carriers and access sessions';
 END IF;
 IF EXISTS (SELECT 1 FROM paperboat.user_machine_pairings p
            JOIN paperboat.user_machines m ON m.id=p.user_machine_id
            WHERE m.environment_id<>m.id
              AND p.installation_state IN ('approved','material_issued','installing','connecting')) THEN
  RAISE EXCEPTION 'machine scope cutover requires drained incomplete installations and protected client journals';
 END IF;
 IF EXISTS (SELECT 1 FROM paperboat.usage_accounting_authorities a
            JOIN paperboat.user_machines m ON m.environment_id=a.environment_id
            WHERE m.environment_id<>m.id AND a.retain_until>now()) THEN
  RAISE EXCEPTION 'machine scope cutover requires completed usage accounting deliveries';
 END IF;
 IF EXISTS (SELECT 1 FROM paperboat.control_config_repository_access_operations a
            JOIN paperboat.user_machines m ON m.environment_id=a.environment_id
            WHERE m.environment_id<>m.id AND a.revoked_at IS NULL
              AND a.state IN ('pending','issued','uncertain')) THEN
  RAISE EXCEPTION 'machine scope cutover requires revoked external repository access grants';
 END IF;
CREATE TEMP TABLE machine_control_scope_rekeys ON COMMIT DROP AS
 SELECT environment_id AS previous_scope, id AS machine_id
 FROM paperboat.user_machines WHERE environment_id<>id;
CREATE UNIQUE INDEX ON machine_control_scope_rekeys(previous_scope);
CREATE UNIQUE INDEX ON machine_control_scope_rekeys(machine_id);
END $$;
-- +goose StatementEnd

-- Encrypted/signed replay material cannot be retargeted. Remove only ephemeral
-- grants and renewal responses; helper keys and incarnation history survive.
DELETE FROM paperboat.control_helper_enrollments h USING machine_control_scope_rekeys r
 WHERE h.environment_id=r.previous_scope;
DELETE FROM paperboat.helper_identity_renewals h USING machine_control_scope_rekeys r
 WHERE h.environment_id=r.previous_scope;
DELETE FROM paperboat.runtime_carrier_attachments a USING machine_control_scope_rekeys r
 WHERE a.machine_id=r.machine_id;
UPDATE paperboat.control_connector_generations c
 SET state=CASE WHEN c.state IN ('admitted','draining') THEN 'pending' ELSE c.state END,
 edge_node_id=NULL,admission_jti_hash=NULL,admission_operation_key=NULL,
 admission_request_hash=NULL,admission_credential_ciphertext=NULL,expires_at=NULL
 FROM machine_control_scope_rekeys r WHERE c.environment_id=r.previous_scope;
UPDATE paperboat.user_machine_pairings p SET installation_config_ciphertext=NULL,
 installation_config_nonce=NULL, installation_recovery_operation_key=NULL FROM machine_control_scope_rekeys r
 WHERE p.user_machine_id=r.machine_id;
UPDATE paperboat.control_config_credentials c SET revoked_at=coalesce(c.revoked_at,now())
 FROM machine_control_scope_rekeys r WHERE c.environment_id=r.previous_scope;
UPDATE paperboat.control_config_repository_lease_authority c
 SET revoked_at=coalesce(c.revoked_at,now()),version=c.version+1,updated_at=now()
 FROM machine_control_scope_rekeys r
 WHERE c.environment_id=r.previous_scope AND c.lease_id IS NOT NULL;

-- Temporarily defer exactly the scope-related foreign keys. Restore their
-- original deferrability after all dependent rows and the parent have moved.
-- +goose StatementBegin
DO $$ DECLARE constraint_row record; scoped_table record;
BEGIN
CREATE TEMP TABLE machine_control_scope_constraints ON COMMIT DROP AS
 SELECT c.conrelid,c.conname,c.condeferrable,c.condeferred
 FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid
 WHERE c.contype='f' AND t.relnamespace='paperboat'::regnamespace
 AND (c.confrelid='paperboat.control_environments'::regclass OR EXISTS (
  SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.conrelid
   AND a.attnum=ANY(c.conkey) AND a.attname='environment_id'));
 FOR constraint_row IN SELECT * FROM machine_control_scope_constraints LOOP
  EXECUTE format('ALTER TABLE %s ALTER CONSTRAINT %I DEFERRABLE INITIALLY DEFERRED',
                 constraint_row.conrelid::regclass,constraint_row.conname);
 END LOOP;
 SET CONSTRAINTS ALL DEFERRED;
 UPDATE paperboat.control_environments e SET id=r.machine_id,workspace_id=r.machine_id
 FROM machine_control_scope_rekeys r WHERE e.id=r.previous_scope;
 -- Move dependent scope references before the owning machine projection.
 -- Every scoped text reference participates, including receipt/history rows.
 FOR scoped_table IN
  SELECT t.oid,t.relname FROM pg_class t JOIN pg_attribute a ON a.attrelid=t.oid
  WHERE t.relnamespace='paperboat'::regnamespace AND t.relkind='r'
    AND a.attname='environment_id' AND NOT a.attisdropped
  ORDER BY (t.relname='user_machines'),t.relname
 LOOP
  EXECUTE format('UPDATE %s s SET environment_id=r.machine_id FROM machine_control_scope_rekeys r WHERE s.environment_id=r.previous_scope',scoped_table.oid::regclass);
 END LOOP;
 SET CONSTRAINTS ALL IMMEDIATE;
 FOR constraint_row IN SELECT * FROM machine_control_scope_constraints LOOP
  EXECUTE format('ALTER TABLE %s ALTER CONSTRAINT %I %s',
   constraint_row.conrelid::regclass,constraint_row.conname,
   CASE WHEN NOT constraint_row.condeferrable THEN 'NOT DEFERRABLE'
        WHEN constraint_row.condeferred THEN 'DEFERRABLE INITIALLY DEFERRED'
        ELSE 'DEFERRABLE INITIALLY IMMEDIATE' END);
 END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 RAISE EXCEPTION 'machine scope consolidation is forward-only; roll back the release before its cutover';
END $$;
-- +goose StatementEnd
