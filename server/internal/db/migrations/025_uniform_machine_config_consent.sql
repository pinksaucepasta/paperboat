-- +goose Up
ALTER TABLE paperboat.control_config_assignments ADD COLUMN configuration_revision text NOT NULL DEFAULT '' CHECK (configuration_revision = '' OR configuration_revision ~ '^[a-f0-9]{64}$');
-- Existing assignments lack the file-derived configuration projection fence.
SELECT paperboat.revoke_config_sync_for_environment(environment_id)
FROM paperboat.control_config_assignments WHERE configuration_revision = '';
UPDATE paperboat.control_config_assignments
SET consent_state = 'revoked', revoked_at = coalesce(revoked_at, now()),
    accepted_at = NULL, approved_pull_revision = NULL, approved_at = NULL,
    version = version + 1, updated_at = now()
WHERE configuration_revision = '';
ALTER TABLE paperboat.control_config_assignments
  ALTER COLUMN consent_state SET DEFAULT 'revoked',
  DROP CONSTRAINT control_config_assignments_consent_state_check,
  ADD CONSTRAINT control_config_assignments_consent_state_check
    CHECK (consent_state IN ('pending', 'accepted', 'revoked')),
  ADD CONSTRAINT control_config_assignment_accepted_projection CHECK (consent_state <> 'accepted' OR configuration_revision <> '');

-- +goose Down
-- Rolling back cannot reinstate credentials or bypass assignments.
ALTER TABLE paperboat.control_config_assignments DROP CONSTRAINT control_config_assignment_accepted_projection, DROP COLUMN configuration_revision;
ALTER TABLE paperboat.control_config_assignments
  ALTER COLUMN consent_state SET DEFAULT 'not_required',
  DROP CONSTRAINT control_config_assignments_consent_state_check,
  ADD CONSTRAINT control_config_assignments_consent_state_check
    CHECK (consent_state IN ('not_required', 'pending', 'accepted', 'revoked'));
