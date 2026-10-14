-- +goose Up
CREATE TABLE paperboat.operator_accounts (
    id text PRIMARY KEY CHECK (id ~ '^opr_[a-f0-9]{32}$'),
    email text NOT NULL UNIQUE,
    password_hash text NOT NULL,
    role text NOT NULL DEFAULT 'admin' CHECK (role IN ('admin','operations','billing','support')),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
    failed_attempts integer NOT NULL DEFAULT 0,
    locked_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (email = lower(email))
);

CREATE TABLE paperboat.operator_bootstrap (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    consumed_at timestamptz NOT NULL,
    operator_id text NOT NULL REFERENCES paperboat.operator_accounts(id)
);

CREATE TABLE paperboat.operator_sessions (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    operator_id text NOT NULL REFERENCES paperboat.operator_accounts(id) ON DELETE CASCADE,
    csrf_hash bytea NOT NULL CHECK (octet_length(csrf_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX operator_sessions_operator_id ON paperboat.operator_sessions(operator_id);

-- Existing staff operation receipts predate separate operator identities. Their
-- actor column is retained for v1 receipts; only operator IDs may be written
-- after this migration. The check keeps that reference live without inserting
-- fake customer rows. Historic customer-backed receipts remain readable.
-- +goose StatementBegin
CREATE FUNCTION paperboat.check_operator_actor() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.actor_user_id IS NOT NULL THEN
    IF NEW.actor_user_id LIKE 'opr_%' THEN
      IF NOT EXISTS (SELECT 1 FROM paperboat.operator_accounts WHERE id = NEW.actor_user_id) THEN
        RAISE EXCEPTION 'unknown operator actor' USING ERRCODE = '23503';
      END IF;
    ELSIF NOT EXISTS (SELECT 1 FROM paperboat.users WHERE id = NEW.actor_user_id) THEN
      RAISE EXCEPTION 'unknown customer actor' USING ERRCODE = '23503';
    END IF;
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION paperboat.check_named_operator_actor() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE actor text := to_jsonb(NEW)->>TG_ARGV[0];
BEGIN
  IF actor LIKE 'opr_%' THEN
    IF NOT EXISTS (SELECT 1 FROM paperboat.operator_accounts WHERE id=actor) THEN
      RAISE EXCEPTION 'unknown operator actor' USING ERRCODE='23503';
    END IF;
  ELSIF actor IS NOT NULL AND NOT EXISTS (SELECT 1 FROM paperboat.users WHERE id=actor) THEN
    RAISE EXCEPTION 'unknown customer actor' USING ERRCODE='23503';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd

ALTER TABLE paperboat.audit_events DROP CONSTRAINT audit_events_actor_user_id_fkey;
ALTER TABLE paperboat.billing_operator_operations DROP CONSTRAINT billing_operator_operations_actor_user_id_fkey;
ALTER TABLE paperboat.billing_uncertain_recoveries DROP CONSTRAINT billing_uncertain_recoveries_actor_user_id_fkey;
ALTER TABLE paperboat.billing_workspace_operations DROP CONSTRAINT billing_workspace_operations_actor_user_id_fkey;
ALTER TABLE paperboat.billing_workspace_recoveries DROP CONSTRAINT billing_workspace_recoveries_actor_user_id_fkey;
ALTER TABLE paperboat.control_operation_recoveries DROP CONSTRAINT control_operation_recoveries_actor_user_id_fkey;
ALTER TABLE paperboat.control_signing_key_revocations DROP CONSTRAINT control_signing_key_revocations_actor_user_id_fkey;
ALTER TABLE paperboat.operator_actions DROP CONSTRAINT operator_actions_actor_user_id_fkey;
ALTER TABLE paperboat.operator_infrastructure_operations DROP CONSTRAINT operator_infrastructure_operations_actor_user_id_fkey;
ALTER TABLE paperboat.release_authority_import_operations DROP CONSTRAINT release_authority_import_operations_actor_user_id_fkey;
ALTER TABLE paperboat.release_authority_bundles DROP CONSTRAINT release_authority_bundles_imported_by_user_id_fkey;
ALTER TABLE paperboat.release_authority_requests DROP CONSTRAINT release_authority_requests_requested_by_user_id_fkey;

CREATE TRIGGER audit_events_operator_actor BEFORE INSERT OR UPDATE OF actor_user_id ON paperboat.audit_events FOR EACH ROW EXECUTE FUNCTION paperboat.check_operator_actor();
CREATE TRIGGER billing_operator_operations_operator_actor BEFORE INSERT OR UPDATE OF actor_user_id ON paperboat.billing_operator_operations FOR EACH ROW EXECUTE FUNCTION paperboat.check_operator_actor();
CREATE TRIGGER billing_uncertain_recoveries_operator_actor BEFORE INSERT OR UPDATE OF actor_user_id ON paperboat.billing_uncertain_recoveries FOR EACH ROW EXECUTE FUNCTION paperboat.check_operator_actor();
CREATE TRIGGER billing_workspace_operations_operator_actor BEFORE INSERT OR UPDATE OF actor_user_id ON paperboat.billing_workspace_operations FOR EACH ROW EXECUTE FUNCTION paperboat.check_operator_actor();
CREATE TRIGGER billing_workspace_recoveries_operator_actor BEFORE INSERT OR UPDATE OF actor_user_id ON paperboat.billing_workspace_recoveries FOR EACH ROW EXECUTE FUNCTION paperboat.check_operator_actor();
CREATE TRIGGER control_operation_recoveries_operator_actor BEFORE INSERT OR UPDATE OF actor_user_id ON paperboat.control_operation_recoveries FOR EACH ROW EXECUTE FUNCTION paperboat.check_operator_actor();
CREATE TRIGGER control_signing_key_revocations_operator_actor BEFORE INSERT OR UPDATE OF actor_user_id ON paperboat.control_signing_key_revocations FOR EACH ROW EXECUTE FUNCTION paperboat.check_operator_actor();
CREATE TRIGGER operator_actions_operator_actor BEFORE INSERT OR UPDATE OF actor_user_id ON paperboat.operator_actions FOR EACH ROW EXECUTE FUNCTION paperboat.check_operator_actor();
CREATE TRIGGER operator_infrastructure_operations_operator_actor BEFORE INSERT OR UPDATE OF actor_user_id ON paperboat.operator_infrastructure_operations FOR EACH ROW EXECUTE FUNCTION paperboat.check_operator_actor();
CREATE TRIGGER release_authority_import_operations_operator_actor BEFORE INSERT OR UPDATE OF actor_user_id ON paperboat.release_authority_import_operations FOR EACH ROW EXECUTE FUNCTION paperboat.check_operator_actor();
CREATE TRIGGER release_authority_bundles_operator_actor BEFORE INSERT OR UPDATE OF imported_by_user_id ON paperboat.release_authority_bundles FOR EACH ROW EXECUTE FUNCTION paperboat.check_named_operator_actor('imported_by_user_id');
CREATE TRIGGER release_authority_requests_operator_actor BEFORE INSERT OR UPDATE OF requested_by_user_id ON paperboat.release_authority_requests FOR EACH ROW EXECUTE FUNCTION paperboat.check_named_operator_actor('requested_by_user_id');
