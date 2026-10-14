-- +goose Up
-- Existing ephemeral access sessions need cleanup by their owning issuer
-- before this cutover. Their credential kind cannot safely be inferred from
-- an identifier, and expiration alone does not prove attachment cleanup.
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM paperboat.user_machine_access_sessions) THEN
  RAISE EXCEPTION 'clean up existing access sessions through their owning issuer and remove them before credential-kind cutover';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE paperboat.user_machine_access_sessions ADD COLUMN credential_kind text NOT NULL
 CHECK (credential_kind IN ('external_issuer','control_plane_signed'));

ALTER TABLE paperboat.diagnostic_upload_intents DROP CONSTRAINT diagnostic_upload_intents_correlation_id_check;
ALTER TABLE paperboat.diagnostic_upload_intents ADD CONSTRAINT diagnostic_upload_intents_correlation_id_check CHECK (correlation_id ~ '^support_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$');
ALTER TABLE paperboat.diagnostic_upload_intents DROP CONSTRAINT diagnostic_upload_intents_id_check;
ALTER TABLE paperboat.diagnostic_upload_intents ADD CONSTRAINT diagnostic_upload_intents_id_check CHECK (id ~ '^[A-Za-z0-9_-]{1,128}$');
ALTER TABLE paperboat.support_reports DROP CONSTRAINT support_reports_support_reference_check;
ALTER TABLE paperboat.support_reports ADD CONSTRAINT support_reports_support_reference_check CHECK (support_reference ~ '^support_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$');
ALTER TABLE paperboat.environment_authorities DROP CONSTRAINT environment_authorities_operation_id_check;
ALTER TABLE paperboat.environment_authorities ADD CONSTRAINT environment_authorities_operation_id_check CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{7,127}$');
ALTER TABLE paperboat.environment_authority_transitions DROP CONSTRAINT environment_authority_transitions_operation_id_check;
ALTER TABLE paperboat.environment_authority_transitions ADD CONSTRAINT environment_authority_transitions_operation_id_check CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{7,127}$');
ALTER TABLE paperboat.environment_authority_transitions DROP CONSTRAINT environment_authority_transitions_abort_operation_id_check;
ALTER TABLE paperboat.environment_authority_transitions ADD CONSTRAINT environment_authority_transitions_abort_operation_id_check CHECK (abort_operation_id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{7,127}$');
ALTER TABLE paperboat.environment_key_enrollment_requests DROP CONSTRAINT environment_key_enrollment_requests_operation_id_check;
ALTER TABLE paperboat.environment_key_enrollment_requests ADD CONSTRAINT environment_key_enrollment_requests_operation_id_check CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{7,127}$');
ALTER TABLE paperboat.environment_scope_manifests DROP CONSTRAINT environment_scope_manifests_operation_id_check;
ALTER TABLE paperboat.environment_scope_manifests ADD CONSTRAINT environment_scope_manifests_operation_id_check CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{7,127}$');
ALTER TABLE paperboat.environment_transition_manifests DROP CONSTRAINT environment_transition_manifests_operation_id_check;
ALTER TABLE paperboat.environment_transition_manifests ADD CONSTRAINT environment_transition_manifests_operation_id_check CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{7,127}$');

ALTER TABLE paperboat.operator_accounts DROP CONSTRAINT operator_accounts_id_check;
ALTER TABLE paperboat.operator_accounts ADD CONSTRAINT operator_accounts_id_check CHECK (id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$');
ALTER TABLE paperboat.peer_endpoint_enrollment_requests DROP CONSTRAINT peer_endpoint_enrollment_requests_id_check;
ALTER TABLE paperboat.peer_endpoint_enrollment_requests ADD CONSTRAINT peer_endpoint_enrollment_requests_id_check CHECK (id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$');

-- Actor receipts are polymorphic references. Resolve their existing principal
-- tables directly; formatting cannot select a trust domain.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.check_operator_actor() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.actor_user_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM paperboat.operator_accounts WHERE id=NEW.actor_user_id)
  AND NOT EXISTS (SELECT 1 FROM paperboat.users WHERE id=NEW.actor_user_id) THEN
  RAISE EXCEPTION 'unknown actor' USING ERRCODE='23503';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.check_named_operator_actor() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE actor text := to_jsonb(NEW)->>TG_ARGV[0];
BEGIN
 IF actor IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM paperboat.operator_accounts WHERE id=actor)
  AND NOT EXISTS (SELECT 1 FROM paperboat.users WHERE id=actor) THEN
  RAISE EXCEPTION 'unknown actor' USING ERRCODE='23503';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- New resource identifiers and authoritative credential kinds cannot safely be undone.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'resource identity migration is forward-only'; END $$;
-- +goose StatementEnd
