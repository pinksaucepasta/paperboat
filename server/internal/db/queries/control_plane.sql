-- name: CreateControlEnvironment :one
INSERT INTO control_environments (id, workspace_id, owner_user_id, desired_state)
VALUES (sqlc.arg(id), sqlc.arg(workspace_id), sqlc.arg(owner_user_id), sqlc.arg(desired_state))
RETURNING *;

-- name: CreateControlHelper :one
INSERT INTO control_helpers (id, environment_id)
VALUES (sqlc.arg(id), sqlc.arg(environment_id))
RETURNING *;

-- name: CreateControlConfigRepository :one
INSERT INTO control_config_repositories
  (id, owner_user_id, provider, external_ref, display_name, provider_account_id,
   external_repository_id, clone_url, publish_url, default_branch, authorization_ref,
   credential_capability, observed_revision)
VALUES
  (sqlc.arg(id), sqlc.arg(owner_user_id), sqlc.arg(provider), sqlc.arg(external_ref), sqlc.arg(display_name),
   sqlc.arg(provider_account_id), sqlc.arg(external_repository_id), sqlc.arg(clone_url), sqlc.arg(publish_url),
   sqlc.arg(default_branch), sqlc.arg(authorization_ref), sqlc.arg(credential_capability), sqlc.narg(observed_revision))
ON CONFLICT (owner_user_id, provider, external_ref) DO UPDATE
SET display_name = EXCLUDED.display_name, provider_account_id = EXCLUDED.provider_account_id,
    external_repository_id = EXCLUDED.external_repository_id, clone_url = EXCLUDED.clone_url,
    publish_url = EXCLUDED.publish_url, default_branch = EXCLUDED.default_branch,
    authorization_ref = EXCLUDED.authorization_ref, credential_capability = EXCLUDED.credential_capability,
    observed_revision = EXCLUDED.observed_revision, state = 'active', disconnected_at = NULL,
    version = control_config_repositories.version + 1, updated_at = now()
WHERE EXCLUDED.provider <> 'git' OR control_config_repositories.state <> 'active'
   OR control_config_repositories.default_branch IS NOT DISTINCT FROM EXCLUDED.default_branch
RETURNING *;

-- name: ListControlConfigRepositories :many
SELECT * FROM control_config_repositories
WHERE owner_user_id = sqlc.arg(owner_user_id) AND state = 'active'
ORDER BY display_name, id LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: GetOwnedControlConfigRepository :one
SELECT * FROM control_config_repositories
WHERE id = sqlc.arg(id) AND owner_user_id = sqlc.arg(owner_user_id) AND state = 'active';

-- name: GetActiveControlConfigRepository :one
SELECT * FROM control_config_repositories
WHERE id = sqlc.arg(id) AND state = 'active';

-- name: DisconnectOwnedControlConfigRepository :one
UPDATE control_config_repositories
SET state = 'disconnected', disconnected_at = sqlc.arg(now), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND owner_user_id = sqlc.arg(owner_user_id) AND state = 'active'
RETURNING *;

-- name: RevokeControlConfigCredentialsForRepository :execrows
UPDATE control_config_credentials c
SET revoked_at = coalesce(c.revoked_at, sqlc.arg(now))
FROM control_config_assignments a
WHERE a.repository_id = sqlc.arg(repository_id) AND c.assignment_id = a.id AND c.revoked_at IS NULL;

-- name: RevokeControlConfigRepositoryAccessForRepository :execrows
UPDATE control_config_repository_access_operations
SET state = 'revoked', revoked_at = coalesce(revoked_at, sqlc.arg(now)), updated_at = sqlc.arg(now)
WHERE repository_id = sqlc.arg(repository_id) AND revoked_at IS NULL AND state IN ('pending','issued','uncertain');

-- name: DisableControlConfigAssignmentsForRepository :execrows
UPDATE control_config_assignments
SET consent_state = 'revoked', accepted_at = NULL, revoked_at = sqlc.arg(now),
    version = version + 1, updated_at = sqlc.arg(now)
WHERE repository_id = sqlc.arg(repository_id) AND revoked_at IS NULL;

-- name: EnsureControlConfigLeaseAuthority :one
INSERT INTO control_config_repository_lease_authority (repository_id)
VALUES (sqlc.arg(repository_id))
ON CONFLICT (repository_id) DO UPDATE SET repository_id = EXCLUDED.repository_id
RETURNING *;

-- name: GetControlConfigLeaseAuthorityForUpdate :one
SELECT * FROM control_config_repository_lease_authority
WHERE repository_id = sqlc.arg(repository_id)
FOR UPDATE;

-- name: GrantControlConfigRepositoryLease :one
UPDATE control_config_repository_lease_authority
SET last_fencing_token = last_fencing_token + 1,
    lease_id = sqlc.arg(lease_id),
    assignment_id = sqlc.arg(assignment_id),
    environment_id = sqlc.arg(environment_id),
    machine_id = sqlc.arg(machine_id),
    installation_generation = sqlc.arg(installation_generation),
    base_remote_revision = sqlc.narg(base_remote_revision),
    operation_id = sqlc.arg(operation_id),
    acquired_at = sqlc.arg(now),
    expires_at = sqlc.arg(expires_at),
    revoked_at = NULL,
    version = version + 1,
    updated_at = sqlc.arg(now)
WHERE repository_id = sqlc.arg(repository_id)
  AND (lease_id IS NULL OR revoked_at IS NOT NULL OR expires_at <= sqlc.arg(now))
RETURNING *;

-- name: RenewControlConfigRepositoryLease :one
UPDATE control_config_repository_lease_authority
SET expires_at = sqlc.arg(expires_at), version = version + 1, updated_at = sqlc.arg(now)
WHERE repository_id = sqlc.arg(repository_id) AND lease_id = sqlc.arg(lease_id)
  AND last_fencing_token = sqlc.arg(fencing_token) AND revoked_at IS NULL
  AND expires_at > sqlc.arg(now)
RETURNING *;

-- name: ReleaseControlConfigRepositoryLease :one
UPDATE control_config_repository_lease_authority
SET lease_id = NULL, assignment_id = NULL, environment_id = NULL, machine_id = NULL,
    installation_generation = NULL, base_remote_revision = NULL, operation_id = NULL,
    acquired_at = NULL, expires_at = NULL, revoked_at = NULL,
    version = version + 1, updated_at = sqlc.arg(now)
WHERE repository_id = sqlc.arg(repository_id) AND lease_id = sqlc.arg(lease_id)
  AND last_fencing_token = sqlc.arg(fencing_token) AND revoked_at IS NULL
RETURNING *;

-- name: RevokeControlConfigRepositoryLease :one
UPDATE control_config_repository_lease_authority
SET revoked_at = sqlc.arg(now), version = version + 1, updated_at = sqlc.arg(now)
WHERE repository_id = sqlc.arg(repository_id) AND lease_id IS NOT NULL AND revoked_at IS NULL
RETURNING *;

-- name: RevokeControlConfigRepositoryLeasesForEnvironment :execrows
UPDATE control_config_repository_lease_authority
SET revoked_at = sqlc.arg(now), version = version + 1, updated_at = sqlc.arg(now)
WHERE environment_id = sqlc.arg(environment_id) AND lease_id IS NOT NULL AND revoked_at IS NULL;

-- name: CreateControlConfigLeaseOperation :one
INSERT INTO control_config_repository_lease_operations
  (operation_id, operation_type, request_hash, repository_id, assignment_id, environment_id,
   machine_id, base_remote_revision, lease_id, fencing_token, result_state, expires_at)
VALUES
  (sqlc.arg(operation_id), sqlc.arg(operation_type), sqlc.arg(request_hash), sqlc.arg(repository_id),
   sqlc.narg(assignment_id), sqlc.narg(environment_id), sqlc.narg(machine_id), sqlc.narg(base_remote_revision),
   sqlc.narg(lease_id), sqlc.narg(fencing_token), sqlc.arg(result_state), sqlc.narg(expires_at))
ON CONFLICT (operation_id) DO NOTHING
RETURNING *;

-- name: GetControlConfigLeaseOperation :one
SELECT * FROM control_config_repository_lease_operations WHERE operation_id = sqlc.arg(operation_id);

-- name: GetControlConfigAssignment :one
SELECT * FROM control_config_assignments WHERE environment_id = $1;

-- name: SetControlConfigAssignment :one
INSERT INTO control_config_assignments (id, machine_id, environment_id, repository_id, push_repository_id, mode, consent_state, warning_revision, automatic_updates, path_rules, repository_bindings, configuration_revision)
SELECT sqlc.arg(assignment_id), machine.id, sqlc.arg(environment_id), sqlc.arg(repository_id),
       sqlc.narg(push_repository_id), sqlc.arg(mode), sqlc.arg(consent_state), sqlc.narg(warning_revision), sqlc.arg(automatic_updates), sqlc.arg(path_rules), sqlc.arg(repository_bindings), sqlc.arg(configuration_revision)
FROM user_machines machine
WHERE machine.environment_id = sqlc.arg(environment_id) AND machine.deleted_at IS NULL
ON CONFLICT (machine_id) DO UPDATE
-- A live assignment keeps its id when its configuration changes: the machine's sync state (last
-- synced baseline, conflicts, comparisons) is keyed by it. A revoked or adopted assignment starts a
-- new lifecycle with a new id.
SET id = CASE WHEN control_config_assignments.revoked_at IS NULL
                  AND control_config_assignments.consent_state <> 'revoked'
                  AND control_config_assignments.adopted_team_id IS NULL
             THEN control_config_assignments.id ELSE EXCLUDED.id END,
    repository_id = EXCLUDED.repository_id, push_repository_id = EXCLUDED.push_repository_id,
    mode = EXCLUDED.mode, consent_state = EXCLUDED.consent_state, automatic_updates = EXCLUDED.automatic_updates,
    path_rules = EXCLUDED.path_rules, repository_bindings = EXCLUDED.repository_bindings,
    configuration_revision = EXCLUDED.configuration_revision,
    adopted_team_id = NULL, adopted_default_version = NULL, approved_pull_revision = NULL, approved_at = NULL,
    warning_revision = EXCLUDED.warning_revision, accepted_at = NULL, revoked_at = NULL,
    version = control_config_assignments.version + 1, updated_at = sqlc.arg(now)
WHERE control_config_assignments.version = sqlc.arg(expected_version)
RETURNING *;

-- name: GetTeamConfigDefault :one
SELECT * FROM team_config_defaults WHERE team_id = sqlc.arg(team_id);

-- name: SetTeamConfigDefault :one
INSERT INTO team_config_defaults (team_id,provider,external_repository_id,display_name,branch,updated_by)
VALUES (sqlc.arg(team_id),sqlc.arg(provider),sqlc.arg(external_repository_id),sqlc.arg(display_name),sqlc.arg(branch),sqlc.arg(updated_by))
ON CONFLICT (team_id) DO UPDATE SET
 provider=EXCLUDED.provider, external_repository_id=EXCLUDED.external_repository_id,
 display_name=EXCLUDED.display_name, branch=EXCLUDED.branch, updated_by=EXCLUDED.updated_by,
 version=team_config_defaults.version+1, updated_at=now()
WHERE team_config_defaults.version=sqlc.arg(expected_version)
RETURNING *;

-- name: DeleteTeamConfigDefault :one
DELETE FROM team_config_defaults d USING teams t
WHERE d.team_id=sqlc.arg(team_id) AND t.team_id=d.team_id
  AND d.version=sqlc.arg(expected_version)
RETURNING d.*;

-- name: GetTeamConfigDefaultAuthority :one
SELECT t.owner_account, t.generation, m.role, m.active
FROM teams t LEFT JOIN team_members m ON m.team_id=t.team_id AND m.account_id=sqlc.arg(account_id)
WHERE t.team_id=sqlc.arg(team_id) AND t.deleted_at IS NULL;

-- name: AdoptTeamConfigDefault :one
INSERT INTO team_config_default_adoptions (team_id,account_id,default_version,repository_id)
SELECT d.team_id, sqlc.arg(account_id), d.version, r.id
FROM team_config_defaults d
JOIN teams t ON t.team_id=d.team_id AND t.deleted_at IS NULL
LEFT JOIN team_members m ON m.team_id=d.team_id AND m.account_id=sqlc.arg(account_id) AND m.active
JOIN control_config_repositories r ON r.owner_user_id=sqlc.arg(account_id)
 AND r.provider=d.provider AND r.external_repository_id=d.external_repository_id
 AND r.state='active' AND r.disconnected_at IS NULL
WHERE d.team_id=sqlc.arg(team_id) AND d.version=sqlc.arg(default_version)
 AND (t.owner_account=sqlc.arg(account_id) OR m.account_id IS NOT NULL)
ON CONFLICT (account_id) DO UPDATE SET team_id=EXCLUDED.team_id,
 default_version=EXCLUDED.default_version, repository_id=EXCLUDED.repository_id, adopted_at=now()
RETURNING *;

-- name: GetTeamConfigDefaultAdoption :one
SELECT a.*, d.provider, d.external_repository_id, d.display_name, d.branch, d.version AS current_default_version
FROM team_config_default_adoptions a JOIN team_config_defaults d USING(team_id)
JOIN teams t ON t.team_id=a.team_id AND t.deleted_at IS NULL
LEFT JOIN team_members m ON m.team_id=a.team_id AND m.account_id=a.account_id AND m.active
WHERE a.account_id=sqlc.arg(account_id) AND (t.owner_account=a.account_id OR m.account_id IS NOT NULL);

-- name: DeleteTeamConfigDefaultAdoption :one
DELETE FROM team_config_default_adoptions WHERE account_id=sqlc.arg(account_id) RETURNING *;

-- name: RevokeAdoptedTeamConfigAssignments :execrows
UPDATE control_config_assignments a SET consent_state='revoked', revoked_at=sqlc.arg(now),
 approved_pull_revision=NULL, approved_at=NULL, version=a.version+1, updated_at=sqlc.arg(now)
FROM user_machines m
WHERE a.machine_id=m.id AND m.user_id=sqlc.arg(account_id)
 AND a.adopted_team_id=sqlc.arg(team_id) AND a.revoked_at IS NULL;

-- name: ApproveControlConfigPullRevision :one
UPDATE control_config_assignments a
SET approved_pull_revision=sqlc.arg(remote_revision), approved_at=sqlc.arg(now),
    version=version+1, updated_at=sqlc.arg(now)
WHERE a.environment_id=sqlc.arg(environment_id) AND a.version=sqlc.arg(expected_version)
  AND a.revoked_at IS NULL AND EXISTS (
    SELECT 1 FROM control_config_sync_statuses s
    WHERE s.environment_id=a.environment_id AND s.assignment_id=a.id
      AND s.remote_revision=sqlc.arg(remote_revision)
  )
RETURNING a.*;

-- name: GetEligibleControlConfigAssignment :one
SELECT a.* FROM control_config_assignments a
JOIN user_machines m ON m.id = a.machine_id
JOIN control_environments e ON e.id = a.environment_id
JOIN users u ON u.id = e.owner_user_id
JOIN control_config_repositories r ON r.id = a.repository_id
WHERE a.environment_id = sqlc.arg(environment_id) AND m.id = sqlc.arg(machine_id)
  AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.installation_generation = sqlc.arg(installation_generation)
  AND a.repository_id IS NOT NULL
  AND a.revoked_at IS NULL AND a.consent_state = 'accepted' AND a.configuration_revision <> ''
  AND e.desired_state = 'active' AND e.revoked_at IS NULL
  AND u.status = 'active'
  AND r.state = 'active' AND r.disconnected_at IS NULL
  AND (a.adopted_team_id IS NULL OR EXISTS (
    SELECT 1 FROM team_config_default_adoptions adoption
    JOIN teams team ON team.team_id=adoption.team_id AND team.deleted_at IS NULL
    LEFT JOIN team_members member ON member.team_id=adoption.team_id AND member.account_id=e.owner_user_id AND member.active
    WHERE adoption.account_id=e.owner_user_id AND adoption.team_id=a.adopted_team_id
      AND adoption.default_version=a.adopted_default_version
      AND (team.owner_account=e.owner_user_id OR member.account_id IS NOT NULL)
  ))
FOR UPDATE OF a, m, e, u, r;

-- name: GetEligibleMachineConfigAssignment :one
SELECT a.* FROM control_config_assignments a
JOIN user_machines m ON m.id = a.machine_id
JOIN control_environments e ON e.id = a.environment_id
JOIN users u ON u.id = e.owner_user_id
JOIN control_config_repositories r ON r.id = a.repository_id
WHERE a.machine_id = sqlc.arg(machine_id) AND a.environment_id = sqlc.arg(environment_id)
  AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.installation_generation = sqlc.arg(installation_generation)
  AND a.repository_id IS NOT NULL
  AND a.revoked_at IS NULL AND a.consent_state = 'accepted' AND a.configuration_revision <> ''
  AND e.desired_state = 'active' AND e.revoked_at IS NULL
  AND u.status = 'active'
  AND r.state = 'active' AND r.disconnected_at IS NULL
  AND (a.adopted_team_id IS NULL OR EXISTS (
    SELECT 1 FROM team_config_default_adoptions adoption
    JOIN teams team ON team.team_id=adoption.team_id AND team.deleted_at IS NULL
    LEFT JOIN team_members member ON member.team_id=adoption.team_id AND member.account_id=e.owner_user_id AND member.active
    WHERE adoption.account_id=e.owner_user_id AND adoption.team_id=a.adopted_team_id
      AND adoption.default_version=a.adopted_default_version
      AND (team.owner_account=e.owner_user_id OR member.account_id IS NOT NULL)
  ))
FOR UPDATE OF a, m, e, u, r;

-- name: CreateControlConfigCredential :one
INSERT INTO control_config_credentials
  (jti, operation_key, request_hash, environment_id, machine_id, assignment_id,
   warning_revision, credential_ciphertext, expires_at)
VALUES
  (sqlc.arg(jti), sqlc.arg(operation_key), sqlc.arg(request_hash), sqlc.arg(environment_id),
   sqlc.arg(machine_id), sqlc.arg(assignment_id), sqlc.narg(warning_revision),
   sqlc.arg(credential_ciphertext), sqlc.arg(expires_at))
ON CONFLICT (operation_key) DO NOTHING
RETURNING *;

-- name: ListRevokedControlCredentialJTIs :many
SELECT revoked.jti FROM (
  SELECT c.jti, c.revoked_at FROM control_config_credentials c
  WHERE c.revoked_at IS NOT NULL AND c.expires_at > sqlc.arg(now)
  UNION ALL
  SELECT m.helper_terminal_session_id AS jti, m.revoked_at FROM user_machine_access_sessions m
  WHERE m.revoked_at IS NOT NULL AND m.expires_at > sqlc.arg(now) AND m.helper_terminal_session_id IS NOT NULL
  UNION ALL
  SELECT m.helper_file_session_id AS jti, m.revoked_at FROM user_machine_access_sessions m
  WHERE m.revoked_at IS NOT NULL AND m.expires_at > sqlc.arg(now) AND m.helper_file_session_id IS NOT NULL
) revoked
ORDER BY revoked.revoked_at, revoked.jti
LIMIT sqlc.arg(row_limit);

-- name: ListRevokedConnectorGenerations :many
SELECT machine_id, connector_id, generation FROM control_connector_generations
WHERE state IN ('revoked','replaced')
ORDER BY updated_at, machine_id, connector_id
LIMIT sqlc.arg(row_limit);

-- name: ListRevokedControlEnvironments :many
SELECT id FROM control_environments
WHERE desired_state = 'revoked'
ORDER BY updated_at, id
LIMIT sqlc.arg(row_limit);

-- name: RevokeControlSigningKey :one
INSERT INTO control_signing_key_revocations (key_id, reason, revoked_at, actor_user_id)
VALUES (sqlc.arg(key_id), sqlc.arg(reason), sqlc.arg(revoked_at), sqlc.arg(actor_user_id))
ON CONFLICT (key_id) DO UPDATE SET reason = EXCLUDED.reason, revoked_at = least(control_signing_key_revocations.revoked_at, EXCLUDED.revoked_at), actor_user_id = EXCLUDED.actor_user_id
RETURNING *;

-- name: ReserveControlSigningKeyRevocation :one
INSERT INTO control_signing_key_revocation_operations (operation_key, key_id, reason)
VALUES (sqlc.arg(operation_key), sqlc.arg(key_id), sqlc.arg(reason))
ON CONFLICT (operation_key) DO NOTHING
RETURNING *;

-- name: GetControlSigningKeyRevocationOperation :one
SELECT * FROM control_signing_key_revocation_operations WHERE operation_key = $1;

-- name: ListRevokedControlSigningKeyIDs :many
SELECT key_id FROM control_signing_key_revocations
ORDER BY revoked_at, key_id
LIMIT sqlc.arg(row_limit);

-- name: GetControlConfigCredentialByOperation :one
SELECT * FROM control_config_credentials WHERE operation_key = $1;

-- name: GetActiveControlConfigCredentialByJTI :one
SELECT * FROM control_config_credentials
WHERE jti = sqlc.arg(jti) AND environment_id = sqlc.arg(environment_id)
  AND machine_id = sqlc.arg(machine_id) AND assignment_id = sqlc.arg(assignment_id)
  AND revoked_at IS NULL AND expires_at > sqlc.arg(now);

-- name: RevokeControlConfigCredentialsForEnvironment :execrows
UPDATE control_config_credentials SET revoked_at = coalesce(revoked_at, sqlc.arg(revoked_at))
WHERE environment_id = sqlc.arg(environment_id) AND revoked_at IS NULL;

-- name: RevokeControlConfigRepositoryAccessForEnvironment :execrows
UPDATE control_config_repository_access_operations
SET state = 'revoked', revoked_at = coalesce(revoked_at, sqlc.arg(now)), updated_at = sqlc.arg(now)
WHERE environment_id = sqlc.arg(environment_id) AND revoked_at IS NULL AND state IN ('pending','issued','uncertain');

-- name: ReserveControlConfigRepositoryAccess :one
INSERT INTO control_config_repository_access_operations
  (operation_id, request_hash, repository_id, assignment_id, environment_id,
   machine_id, installation_generation, warning_revision)
VALUES
  (sqlc.arg(operation_id), sqlc.arg(request_hash), sqlc.arg(repository_id),
   sqlc.arg(assignment_id), sqlc.arg(environment_id), sqlc.arg(machine_id),
   sqlc.arg(installation_generation), sqlc.arg(warning_revision))
ON CONFLICT (operation_id) DO NOTHING
RETURNING *;

-- name: GetControlConfigRepositoryAccessOperation :one
SELECT * FROM control_config_repository_access_operations
WHERE operation_id = sqlc.arg(operation_id);

-- name: CompleteControlConfigRepositoryAccess :one
UPDATE control_config_repository_access_operations
SET state = 'issued', access_ciphertext = sqlc.arg(access_ciphertext),
    expires_at = sqlc.arg(expires_at), updated_at = sqlc.arg(now)
WHERE operation_id = sqlc.arg(operation_id) AND state = 'pending' AND revoked_at IS NULL
RETURNING *;

-- name: MarkControlConfigRepositoryAccessUncertain :execrows
UPDATE control_config_repository_access_operations
SET state = 'uncertain', last_error_code = sqlc.arg(last_error_code), updated_at = sqlc.arg(now)
WHERE operation_id = sqlc.arg(operation_id) AND state = 'pending' AND revoked_at IS NULL;

-- name: ListControlConfigRepositoryAccessPendingProviderRevoke :many
SELECT * FROM control_config_repository_access_operations
WHERE revoked_at IS NOT NULL AND provider_revoked_at IS NULL
  AND access_ciphertext IS NOT NULL AND expires_at > sqlc.arg(now)
ORDER BY revoked_at, operation_id
LIMIT sqlc.arg(row_limit);

-- name: MarkControlConfigRepositoryAccessProviderRevoked :execrows
UPDATE control_config_repository_access_operations
SET provider_revoked_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE operation_id = sqlc.arg(operation_id) AND revoked_at IS NOT NULL AND provider_revoked_at IS NULL;

-- name: RecordControlConfigRepositoryAccessRevokeFailure :execrows
UPDATE control_config_repository_access_operations
SET revoke_attempts = revoke_attempts + 1, last_error_code = sqlc.arg(last_error_code), updated_at = sqlc.arg(now)
WHERE operation_id = sqlc.arg(operation_id) AND revoked_at IS NOT NULL AND provider_revoked_at IS NULL;

-- name: AcceptControlConfigConsent :one
UPDATE control_config_assignments
SET consent_state = 'accepted', warning_revision = sqlc.arg(warning_revision), accepted_at = sqlc.arg(now),
    revoked_at = NULL, version = version + 1, updated_at = sqlc.arg(now)
WHERE environment_id = sqlc.arg(environment_id) AND version = sqlc.arg(expected_version)
  AND repository_id IS NOT NULL AND consent_state = 'pending' AND configuration_revision <> ''
  AND warning_revision = sqlc.arg(warning_revision)
RETURNING *;

-- name: RemoveControlConfigConsent :one
UPDATE control_config_assignments
SET consent_state = 'pending', warning_revision = sqlc.arg(warning_revision), accepted_at = NULL,
    revoked_at = NULL, version = version + 1, updated_at = sqlc.arg(now)
WHERE environment_id = sqlc.arg(environment_id) AND version = sqlc.arg(expected_version)
  AND repository_id IS NOT NULL AND consent_state = 'accepted'
RETURNING *;

-- name: GetControlConfigWarningContext :one
SELECT cm.alias AS machine_name, cm.workspace_root AS canonical_scope,
	   r.display_name AS repository_name, a.mode
FROM control_config_assignments a
JOIN control_environments e ON e.id = a.environment_id
JOIN user_machines cm ON cm.environment_id = e.id AND cm.deleted_at IS NULL
JOIN control_config_repositories r ON r.id = a.repository_id AND r.state = 'active'
WHERE a.environment_id = sqlc.arg(environment_id) AND e.owner_user_id = sqlc.arg(owner_user_id)
  AND e.desired_state = 'active' AND e.revoked_at IS NULL AND a.revoked_at IS NULL;

-- name: ReconcileStaleControlConfigWarning :many
WITH stale AS (
  SELECT a.environment_id
  FROM control_config_assignments a
  JOIN user_machines cm ON cm.environment_id = a.environment_id AND cm.deleted_at IS NULL
  WHERE a.repository_id IS NOT NULL AND a.revoked_at IS NULL
    AND a.consent_state = 'accepted' AND a.configuration_revision <> ''
    AND a.warning_revision IS DISTINCT FROM sqlc.arg(warning_revision)
),
updated_assignments AS (
  UPDATE control_config_assignments a
  SET consent_state = 'pending', warning_revision = sqlc.arg(warning_revision), accepted_at = NULL,
      version = version + 1, updated_at = sqlc.arg(now)
  FROM stale
  WHERE a.environment_id = stale.environment_id
  RETURNING a.environment_id
),
revoked_credentials AS (
  UPDATE control_config_credentials c
  SET revoked_at = coalesce(c.revoked_at, sqlc.arg(now))
  FROM updated_assignments u
  WHERE c.environment_id = u.environment_id AND c.revoked_at IS NULL
  RETURNING c.environment_id
),
revoked_access AS (
  UPDATE control_config_repository_access_operations access
  SET state = 'revoked', revoked_at = coalesce(access.revoked_at, sqlc.arg(now)), updated_at = sqlc.arg(now)
  FROM updated_assignments u
  WHERE access.environment_id = u.environment_id AND access.revoked_at IS NULL
    AND access.state IN ('pending','issued','uncertain')
  RETURNING access.environment_id
),
revoked_leases AS (
  UPDATE control_config_repository_lease_authority l
  SET revoked_at = sqlc.arg(now), version = version + 1, updated_at = sqlc.arg(now)
  FROM updated_assignments u
  WHERE l.environment_id = u.environment_id AND l.lease_id IS NOT NULL AND l.revoked_at IS NULL
  RETURNING l.environment_id
)
SELECT environment_id FROM updated_assignments ORDER BY environment_id;

-- name: RecordControlConfigSyncStatus :one
INSERT INTO control_config_sync_statuses
  (environment_id, repository_id, assignment_id, machine_id, installation_generation,
   warning_revision, policy_revision, sync_revision, state, mode,
   remote_revision, manifest_health, manifest_revision, managed_path_count,
   pending_clean_path_count, last_applied_revision, last_published_revision,
   lease_id, fencing_token,
   skipped, conflicts, review, error_code, recovery_actions, last_attempt_at,
   last_successful_at, machine_updated_at, observed_at)
VALUES
  (sqlc.arg(environment_id), sqlc.arg(repository_id), sqlc.arg(assignment_id), sqlc.arg(machine_id),
   sqlc.arg(installation_generation), sqlc.arg(warning_revision), sqlc.arg(policy_revision),
   sqlc.arg(sync_revision), sqlc.arg(state), sqlc.arg(mode),
   sqlc.narg(remote_revision), sqlc.arg(manifest_health), sqlc.narg(manifest_revision),
   sqlc.arg(managed_path_count), sqlc.arg(pending_clean_path_count),
   sqlc.narg(last_applied_revision), sqlc.narg(last_published_revision),
   sqlc.narg(lease_id), sqlc.narg(fencing_token), sqlc.arg(skipped),
   sqlc.arg(conflicts), sqlc.arg(review), sqlc.narg(error_code), sqlc.arg(recovery_actions),
   sqlc.narg(last_attempt_at), sqlc.narg(last_successful_at), sqlc.arg(machine_updated_at), sqlc.arg(observed_at))
ON CONFLICT (environment_id) DO UPDATE
SET repository_id = EXCLUDED.repository_id, assignment_id = EXCLUDED.assignment_id,
    machine_id = EXCLUDED.machine_id, installation_generation = EXCLUDED.installation_generation,
    warning_revision = EXCLUDED.warning_revision, policy_revision = EXCLUDED.policy_revision,
    sync_revision = EXCLUDED.sync_revision,
    state = EXCLUDED.state, mode = EXCLUDED.mode, remote_revision = EXCLUDED.remote_revision,
    manifest_health = EXCLUDED.manifest_health, manifest_revision = EXCLUDED.manifest_revision,
    managed_path_count = EXCLUDED.managed_path_count,
    pending_clean_path_count = EXCLUDED.pending_clean_path_count,
    last_applied_revision = EXCLUDED.last_applied_revision,
    last_published_revision = EXCLUDED.last_published_revision,
    lease_id = EXCLUDED.lease_id, fencing_token = EXCLUDED.fencing_token,
    skipped = EXCLUDED.skipped, conflicts = EXCLUDED.conflicts, review = EXCLUDED.review,
    error_code = EXCLUDED.error_code,
    recovery_actions = EXCLUDED.recovery_actions, last_attempt_at = EXCLUDED.last_attempt_at,
    last_successful_at = EXCLUDED.last_successful_at, machine_updated_at = EXCLUDED.machine_updated_at,
    observed_at = EXCLUDED.observed_at
WHERE (
    control_config_sync_statuses.sync_revision < EXCLUDED.sync_revision
    OR (
      control_config_sync_statuses.sync_revision = EXCLUDED.sync_revision
      AND control_config_sync_statuses.machine_updated_at < EXCLUDED.machine_updated_at
    )
  )
  AND EXISTS (
    SELECT 1 FROM control_config_assignments current_assignment
    WHERE current_assignment.environment_id = EXCLUDED.environment_id
      AND current_assignment.id = EXCLUDED.assignment_id
      AND current_assignment.machine_id = EXCLUDED.machine_id
  )
  AND control_config_sync_statuses.machine_id = EXCLUDED.machine_id
  AND control_config_sync_statuses.installation_generation = EXCLUDED.installation_generation
RETURNING *;

-- name: GetControlConfigSyncRevision :one
SELECT control_config_sync_statuses.sync_revision
FROM control_config_sync_statuses
WHERE control_config_sync_statuses.environment_id = sqlc.arg(environment_id)
  AND EXISTS (
    SELECT 1 FROM control_config_assignments current_assignment
    WHERE current_assignment.environment_id = control_config_sync_statuses.environment_id
      AND current_assignment.id = sqlc.arg(assignment_id)
      AND current_assignment.machine_id = control_config_sync_statuses.machine_id
  )
  AND control_config_sync_statuses.machine_id = sqlc.arg(machine_id)
  AND control_config_sync_statuses.installation_generation = sqlc.arg(installation_generation);

-- name: ListOwnedControlConfigSyncStatus :many
SELECT
  environment.id AS environment_id,
  machine.id AS machine_id,
  environment.workspace_id,
  COALESCE(machine.alias, environment.id)::text AS alias,
  environment.desired_state AS environment_state,
  assignment.id AS assignment_id,
  assignment.repository_id,
	assignment.push_repository_id,
	assignment.automatic_updates,
	assignment.approved_pull_revision,
	assignment.adopted_team_id,
	assignment.adopted_default_version,
	assignment.mode,
  assignment.consent_state,
  assignment.warning_revision,
  assignment.version AS assignment_version,
  repository.display_name AS repository_name,
  repository.state AS repository_state,
  COALESCE(machine.installation_generation, 0)::bigint AS installation_generation,
  status.state AS sync_state,
  status.assignment_id AS status_assignment_id,
  status.repository_id AS status_repository_id,
  status.machine_id AS status_machine_id,
  status.installation_generation AS status_installation_generation,
  status.policy_revision,
  status.sync_revision,
  status.remote_revision,
  status.manifest_health,
  status.manifest_revision,
  status.managed_path_count,
  status.pending_clean_path_count,
  status.last_applied_revision,
  status.last_published_revision,
  COALESCE(status.skipped, '[]'::jsonb)::jsonb AS skipped,
  COALESCE(status.conflicts, '[]'::jsonb)::jsonb AS conflicts,
  COALESCE(status.review, '[]'::jsonb)::jsonb AS review,
  status.error_code,
  COALESCE(status.recovery_actions, '[]'::jsonb)::jsonb AS recovery_actions,
  status.last_attempt_at,
  status.last_successful_at,
  status.machine_updated_at,
  status.observed_at
FROM control_environments environment
LEFT JOIN control_config_assignments assignment
  ON assignment.environment_id = environment.id
LEFT JOIN control_config_repositories repository
  ON repository.id = assignment.repository_id
LEFT JOIN control_config_sync_statuses status
  ON status.environment_id = environment.id
 AND status.assignment_id = assignment.id AND assignment.revoked_at IS NULL
LEFT JOIN user_machines machine
  ON machine.environment_id = environment.id AND machine.deleted_at IS NULL
WHERE environment.owner_user_id = sqlc.arg(owner_user_id)
  AND environment.desired_state <> 'revoked'
ORDER BY lower(COALESCE(machine.alias, environment.id)), environment.id;

-- name: ClearControlConfigAssignment :one
UPDATE control_config_assignments
SET repository_id = NULL, consent_state = 'revoked', warning_revision = NULL, accepted_at = NULL,
    revoked_at = sqlc.arg(now), version = version + 1, updated_at = sqlc.arg(now)
WHERE environment_id = sqlc.arg(environment_id) AND version = sqlc.arg(expected_version)
RETURNING *;

-- name: CreateControlHelperEnrollment :one
INSERT INTO control_helper_enrollments (id, environment_id, helper_id, jti_hash, operation_key, request_hash, grant_ciphertext, expires_at)
VALUES (sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(helper_id), sqlc.arg(jti_hash), sqlc.arg(operation_key), sqlc.arg(request_hash), sqlc.arg(grant_ciphertext), sqlc.arg(expires_at))
ON CONFLICT (operation_key) DO NOTHING
RETURNING *;

-- name: GetControlHelperEnrollmentByOperationKey :one
SELECT * FROM control_helper_enrollments WHERE operation_key = $1;

-- name: GetPendingControlHelperEnrollmentForEnvironment :one
SELECT * FROM control_helper_enrollments
WHERE environment_id = sqlc.arg(environment_id) AND state = 'pending' AND revoked_at IS NULL
ORDER BY created_at DESC LIMIT 1;

-- name: GetActiveControlHelperForEnvironment :one
SELECT * FROM control_helpers
WHERE environment_id = sqlc.arg(environment_id) AND state = 'active' AND revoked_at IS NULL
ORDER BY updated_at DESC LIMIT 1;

-- name: GetPendingControlHelperForEnvironment :one
SELECT * FROM control_helpers
WHERE environment_id = sqlc.arg(environment_id) AND state = 'pending' AND revoked_at IS NULL
ORDER BY updated_at DESC LIMIT 1;

-- name: RevokeExpiredControlHelperEnrollments :execrows
UPDATE control_helper_enrollments
SET state='revoked', revoked_at=sqlc.arg(now)
WHERE environment_id=sqlc.arg(environment_id) AND state='pending' AND expires_at<=sqlc.arg(now) AND revoked_at IS NULL;

-- name: RevokeUnboundAuthenticatedHostHelperEnrollment :one
WITH revoked AS (
  UPDATE control_helper_enrollments AS enrollment
  SET state = 'revoked', revoked_at = coalesce(enrollment.revoked_at, sqlc.arg(now))
  WHERE enrollment.id = sqlc.arg(helper_enrollment_id)
    AND enrollment.environment_id = sqlc.arg(environment_id)
    AND enrollment.state = 'pending'
    AND enrollment.revoked_at IS NULL
    AND NOT EXISTS (
      SELECT 1
      FROM user_machine_pairings AS pairing
      WHERE pairing.authenticated_setup_helper_enrollment_id = enrollment.id
        AND pairing.authenticated_setup_cli_session_id IS NOT NULL
        AND pairing.state IN ('approved', 'consumed')
    )
  RETURNING enrollment.id
)
SELECT (
  EXISTS (SELECT 1 FROM revoked)
  OR NOT EXISTS (
    SELECT 1 FROM control_helper_enrollments AS candidate
    WHERE candidate.id = sqlc.arg(helper_enrollment_id)
  )
  OR EXISTS (
    SELECT 1 FROM control_helper_enrollments AS candidate
    WHERE candidate.id = sqlc.arg(helper_enrollment_id)
      AND (
        candidate.state IN ('expired', 'revoked')
        OR (candidate.state = 'pending'
          AND (candidate.revoked_at IS NOT NULL OR candidate.expires_at <= sqlc.arg(now)))
        OR (candidate.state = 'consumed' AND NOT EXISTS (
          SELECT 1
          FROM control_helpers AS helper
          JOIN control_environments AS environment ON environment.id = helper.environment_id
          WHERE helper.id = candidate.helper_id
            AND helper.environment_id = candidate.environment_id
            AND helper.state = 'active'
            AND helper.revoked_at IS NULL
            AND environment.desired_state = 'active'
            AND environment.revoked_at IS NULL
        ))
      )
  )
  OR EXISTS (
    SELECT 1
    FROM user_machine_pairings AS pairing
    JOIN user_machines AS machine ON machine.id = pairing.user_machine_id
    JOIN control_helper_enrollments AS enrollment
      ON enrollment.id = pairing.authenticated_setup_helper_enrollment_id
     AND enrollment.environment_id = machine.environment_id
    WHERE enrollment.id = sqlc.arg(helper_enrollment_id)
      AND pairing.authenticated_setup_cli_session_id IS NOT NULL
      AND pairing.state IN ('approved', 'consumed')
  )
)::boolean AS safe;

-- name: ConsumeControlHelperEnrollment :one
UPDATE control_helper_enrollments AS enrollment
SET state = 'consumed', consumed_at = sqlc.arg(now)
WHERE enrollment.id = sqlc.arg(id) AND enrollment.jti_hash = sqlc.arg(jti_hash) AND enrollment.state = 'pending'
  AND enrollment.expires_at > sqlc.arg(now) AND enrollment.revoked_at IS NULL
  AND EXISTS (SELECT 1 FROM control_environments e WHERE e.id = enrollment.environment_id AND e.desired_state = 'active' AND e.revoked_at IS NULL)
RETURNING *;

-- name: GetConsumedControlHelperEnrollmentForReplay :one
SELECT enrollment.*
FROM control_helper_enrollments AS enrollment
JOIN control_helpers AS helper
  ON helper.id = enrollment.helper_id
 AND helper.environment_id = enrollment.environment_id
JOIN control_environments AS environment
  ON environment.id = enrollment.environment_id
WHERE enrollment.id = sqlc.arg(id)
  AND enrollment.jti_hash = sqlc.arg(jti_hash)
  AND enrollment.state = 'consumed'
  AND enrollment.consumed_at IS NOT NULL
  AND enrollment.expires_at > sqlc.arg(now)
  AND helper.state = 'active'
  AND helper.revoked_at IS NULL
  AND environment.desired_state = 'active'
  AND environment.revoked_at IS NULL
FOR UPDATE OF enrollment, helper;

-- name: ActivateControlHelper :one
UPDATE control_helpers
SET state = 'active', key_thumbprint = sqlc.arg(key_thumbprint), public_key = sqlc.arg(public_key), last_seen_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND environment_id = sqlc.arg(environment_id) AND state = 'pending'
RETURNING *;

-- name: ReplaceControlHelper :one
UPDATE control_helpers
SET state = 'replaced', generation = generation + 1, replacement_operation_key = sqlc.arg(operation_key),
    revoked_at = sqlc.arg(revoked_at), updated_at = sqlc.arg(revoked_at)
WHERE id = sqlc.arg(id) AND environment_id = sqlc.arg(environment_id) AND state = 'active'
RETURNING *;

-- name: GetControlHelperForUpdate :one
SELECT * FROM control_helpers WHERE id = sqlc.arg(id) AND environment_id = sqlc.arg(environment_id) FOR UPDATE;

-- name: GetActiveControlHelper :one
SELECT * FROM control_helpers
WHERE id = sqlc.arg(id) AND environment_id = sqlc.arg(environment_id)
  AND state = 'active' AND revoked_at IS NULL;

-- name: GetMachineIDForActiveHelper :one
SELECT (SELECT m.id FROM user_machines m
 WHERE m.environment_id = h.environment_id AND m.deleted_at IS NULL
 ORDER BY m.created_at DESC LIMIT 1)::text AS machine_id
FROM control_helpers h
WHERE h.id = sqlc.arg(helper_id) AND h.environment_id = sqlc.arg(environment_id)
  AND h.state = 'active' AND h.revoked_at IS NULL;

-- name: SetControlHelperReplacementGeneration :one
UPDATE control_helpers SET replacement_connector_generation = sqlc.arg(connector_generation), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND replacement_operation_key = sqlc.arg(operation_key)
RETURNING *;

-- name: RevokePendingHelperEnrollments :execrows
UPDATE control_helper_enrollments
SET state = 'revoked', revoked_at = sqlc.arg(revoked_at)
WHERE helper_id = sqlc.arg(helper_id) AND state = 'pending';

-- name: RevokeControlHelpersForEnvironment :execrows
UPDATE control_helpers
SET state = 'revoked', revoked_at = coalesce(revoked_at, sqlc.arg(revoked_at)),
    updated_at = sqlc.arg(revoked_at)
WHERE environment_id = sqlc.arg(environment_id)
  AND state IN ('pending','active') AND revoked_at IS NULL;

-- name: RevokeControlHelperEnrollmentsForEnvironment :execrows
UPDATE control_helper_enrollments
SET state = 'revoked', revoked_at = coalesce(revoked_at, sqlc.arg(revoked_at))
WHERE environment_id = sqlc.arg(environment_id)
  AND state = 'pending' AND revoked_at IS NULL;

-- name: RevokeControlConnectorForEnvironment :execrows
UPDATE control_connector_generations
SET state = 'revoked', revoked_at = coalesce(revoked_at, sqlc.arg(revoked_at)),
    version = version + 1, updated_at = sqlc.arg(revoked_at)
WHERE environment_id = sqlc.arg(environment_id)
  AND state IN ('pending','admitted','draining') AND revoked_at IS NULL;

-- name: AdvanceControlConnectorGeneration :one
INSERT INTO control_connector_generations (environment_id, connector_id, machine_id, generation, edge_pool, state)
VALUES (sqlc.arg(environment_id), sqlc.arg(connector_id), sqlc.arg(machine_id), 1, sqlc.arg(edge_pool), 'pending')
ON CONFLICT (environment_id, connector_id) DO UPDATE
SET machine_id = EXCLUDED.machine_id, generation = control_connector_generations.generation + 1,
    edge_pool = EXCLUDED.edge_pool, state = 'pending', edge_node_id = NULL,
    admission_jti_hash = NULL, expires_at = NULL, revoked_at = NULL,
    version = control_connector_generations.version + 1, updated_at = sqlc.arg(updated_at)
RETURNING *;

-- name: BindControlConnectorMachine :one
INSERT INTO control_connector_generations (environment_id, connector_id, machine_id, generation, edge_pool, state)
VALUES (sqlc.arg(environment_id), sqlc.arg(connector_id), sqlc.arg(machine_id), 1, sqlc.arg(edge_pool), 'pending')
ON CONFLICT (environment_id, connector_id) DO UPDATE
SET machine_id = EXCLUDED.machine_id, state = 'pending', updated_at = sqlc.arg(updated_at)
RETURNING *;

-- name: EnsureControlConnectorMachine :one
INSERT INTO control_connector_generations (environment_id, connector_id, machine_id, generation, edge_pool, state)
VALUES (sqlc.arg(environment_id), sqlc.arg(connector_id), sqlc.arg(machine_id), 1, sqlc.arg(edge_pool), 'pending')
ON CONFLICT (environment_id, connector_id) DO UPDATE
SET environment_id = EXCLUDED.environment_id
RETURNING *;

-- name: GetControlEnvironment :one
SELECT * FROM control_environments WHERE id = $1;

-- name: RevokeControlRoutesForEnvironment :execrows
UPDATE control_routes
SET desired_state = CASE WHEN applied_node_id IS NULL THEN 'detached' ELSE 'detaching' END,
    desired_revision = desired_revision + 1, version = version + 1, updated_at = sqlc.arg(now)
WHERE environment_id = sqlc.arg(environment_id)
  AND desired_state NOT IN ('detaching','detached');

-- name: UpdateControlEnvironmentDesiredState :one
UPDATE control_environments
SET desired_state = sqlc.arg(desired_state), desired_version = desired_version + 1,
    revoked_at = CASE WHEN sqlc.arg(desired_state)::text = 'revoked' THEN coalesce(revoked_at, sqlc.arg(now)) ELSE revoked_at END,
    updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND desired_version = sqlc.arg(expected_version)
RETURNING *;

-- name: ApplyControlEnvironmentState :one
UPDATE control_environments
SET applied_state = sqlc.arg(applied_state), applied_version = sqlc.arg(desired_version), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND desired_version = sqlc.arg(desired_version)
  AND applied_version < sqlc.arg(desired_version)
RETURNING *;

-- name: ReserveControlOperation :one
INSERT INTO control_operations (id, operation_key, operation_type, request_hash)
VALUES (sqlc.arg(id), sqlc.arg(operation_key), sqlc.arg(operation_type), sqlc.arg(request_hash))
ON CONFLICT (operation_key) DO NOTHING
RETURNING *;

-- name: GetControlOperationByKey :one
SELECT * FROM control_operations WHERE operation_key = $1;

-- name: GetControlPlaneQueueMetrics :one
SELECT
  count(*) FILTER (WHERE state IN ('pending','failed','uncertain') OR (state = 'running' AND lease_expires_at <= now()))::bigint AS operation_depth,
  CAST(coalesce(extract(epoch FROM (now() - min(created_at) FILTER (WHERE state IN ('pending','failed','uncertain') OR (state = 'running' AND lease_expires_at <= now())))), 0) AS bigint) AS operation_oldest_age_seconds,
  count(*) FILTER (WHERE state = 'dead_letter')::bigint AS operation_dead_letter_depth,
  (SELECT count(*) FROM control_reconciliation_attempts WHERE state IN ('started','uncertain'))::bigint AS reconciliation_depth,
  CAST(coalesce((SELECT extract(epoch FROM (now() - min(started_at))) FROM control_reconciliation_attempts WHERE state IN ('started','uncertain')), 0) AS bigint) AS reconciliation_oldest_age_seconds,
  (SELECT count(*) FROM control_tunnel_nodes WHERE state IN ('registered','ready') AND (last_heartbeat_at IS NULL OR last_heartbeat_at <= now() - interval '2 minutes'))::bigint AS stale_node_depth
FROM control_operations;

-- name: GetConfigSyncMetrics :one
SELECT
  (SELECT count(*)
   FROM control_config_assignments assignment
   JOIN control_environments environment ON environment.id=assignment.environment_id
   JOIN control_config_repositories repository ON repository.id=assignment.repository_id
   WHERE environment.desired_state='active'
     AND repository.state='active'
     AND assignment.consent_state = 'accepted')::bigint AS eligible_environments,
  (SELECT count(*) FROM control_config_sync_statuses WHERE state IN ('restoring','watching','pending','syncing','healthy','warning','conflict','sync_uncertain'))::bigint AS reporting_environments,
  (SELECT count(*) FROM control_config_sync_statuses WHERE state='conflict')::bigint AS conflict_environments,
  (SELECT coalesce(sum(pending_clean_path_count),0) FROM control_config_sync_statuses)::bigint AS pending_paths,
  (SELECT count(*) FROM control_config_sync_statuses WHERE state='sync_uncertain')::bigint AS uncertain_publications,
  (SELECT count(*) FROM control_config_repository_lease_authority
   WHERE lease_id IS NOT NULL AND revoked_at IS NULL AND expires_at>now())::bigint AS active_writer_leases,
  (SELECT count(*) FROM control_config_repository_lease_operations WHERE result_state='busy')::bigint AS lease_contention_total,
  (SELECT count(*) FROM control_config_conflict_resolutions WHERE state='pending')::bigint AS pending_resolutions,
  CAST(coalesce((SELECT extract(epoch FROM (now()-min(requested_at)))
                 FROM control_config_conflict_resolutions WHERE state='pending'),0) AS bigint) AS oldest_pending_resolution_age_seconds,
  (SELECT count(*) FROM control_config_repository_access_operations
   WHERE revoked_at IS NOT NULL AND provider_revoked_at IS NULL)::bigint AS pending_provider_revocations;

-- name: LeaseControlOperations :many
UPDATE control_operations
SET state = 'running', attempts = attempts + 1, next_attempt_at = NULL,
    lease_expires_at = sqlc.arg(lease_expires_at), updated_at = sqlc.arg(now)
WHERE id IN (
  SELECT id FROM control_operations
  WHERE (state IN ('pending','failed','uncertain') AND (next_attempt_at IS NULL OR next_attempt_at <= sqlc.arg(now)))
     OR (state = 'running' AND lease_expires_at <= sqlc.arg(now))
  ORDER BY coalesce(next_attempt_at, created_at), created_at
  FOR UPDATE SKIP LOCKED
  LIMIT sqlc.arg(batch_size)
)
RETURNING *;

-- name: CompleteControlOperation :execrows
UPDATE control_operations
SET state = 'succeeded', result = coalesce(sqlc.arg(result)::jsonb, '{}'::jsonb), completed_at = sqlc.arg(now),
    last_error = NULL, lease_expires_at = NULL, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'running' AND lease_expires_at = sqlc.arg(lease_expires_at);

-- name: MarkControlOperationUncertain :execrows
UPDATE control_operations
SET state = 'uncertain', last_error = sqlc.arg(last_error), uncertain_at = coalesce(uncertain_at, sqlc.arg(now)),
    next_attempt_at = sqlc.arg(next_attempt_at), lease_expires_at = NULL, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'running' AND lease_expires_at = sqlc.arg(lease_expires_at);

-- name: MarkControlOperationFailed :execrows
UPDATE control_operations
SET state = CASE WHEN attempts >= sqlc.arg(max_attempts) THEN 'dead_letter' ELSE 'failed' END,
    last_error = sqlc.arg(last_error), next_attempt_at = sqlc.arg(next_attempt_at),
    lease_expires_at = NULL, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'running' AND lease_expires_at = sqlc.arg(lease_expires_at);

-- name: ReserveControlOperationRecovery :one
INSERT INTO control_operation_recoveries (operation_key, operation_id, actor_user_id)
VALUES (sqlc.arg(operation_key), sqlc.arg(operation_id), sqlc.arg(actor_user_id))
ON CONFLICT (operation_key) DO NOTHING
RETURNING *;

-- name: GetControlOperationRecovery :one
SELECT * FROM control_operation_recoveries WHERE operation_key = $1;

-- name: GetHelperIdentityRenewal :one
SELECT * FROM helper_identity_renewals WHERE operation_key=$1;

-- name: CreateHelperIdentityRenewal :one
INSERT INTO helper_identity_renewals (operation_key,helper_id,environment_id,request_hash,identity_ciphertext,expires_at)
VALUES (sqlc.arg(operation_key),sqlc.arg(helper_id),sqlc.arg(environment_id),sqlc.arg(request_hash),sqlc.arg(identity_ciphertext),sqlc.arg(expires_at))
ON CONFLICT (operation_key) DO NOTHING
RETURNING *;

-- name: RecoverDeadLetterControlOperation :execrows
UPDATE control_operations
SET state = 'pending', attempts = 0, last_error = NULL, next_attempt_at = NULL,
    lease_expires_at = NULL, uncertain_at = NULL, completed_at = NULL, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'dead_letter';

-- name: RegisterControlTunnelNode :one
INSERT INTO control_tunnel_nodes (id, edge_pool, protocol_version, process_epoch, endpoint_host, endpoint_tcp_port, endpoint_quic_port, carrier_endpoint_host, carrier_endpoint_tcp_port, carrier_endpoint_quic_port, carrier_server_spki_sha256, carrier_server_certificate_chain_pem, state, ready, capacity, last_heartbeat_at)
VALUES (sqlc.arg(id), sqlc.arg(edge_pool), sqlc.arg(protocol_version), sqlc.arg(process_epoch), sqlc.arg(endpoint_host), sqlc.arg(endpoint_tcp_port), sqlc.arg(endpoint_quic_port), sqlc.narg(carrier_endpoint_host), sqlc.narg(carrier_endpoint_tcp_port), sqlc.narg(carrier_endpoint_quic_port), sqlc.narg(carrier_server_spki_sha256), sqlc.narg(carrier_server_certificate_chain_pem), 'registered', false,
        coalesce(sqlc.arg(capacity)::jsonb, '{}'::jsonb), sqlc.arg(now))
ON CONFLICT (id) DO UPDATE
SET edge_pool = EXCLUDED.edge_pool,
    protocol_version = EXCLUDED.protocol_version, process_epoch = EXCLUDED.process_epoch,
    endpoint_host = EXCLUDED.endpoint_host, endpoint_tcp_port = EXCLUDED.endpoint_tcp_port, endpoint_quic_port = EXCLUDED.endpoint_quic_port,
    carrier_endpoint_host = EXCLUDED.carrier_endpoint_host, carrier_endpoint_tcp_port = EXCLUDED.carrier_endpoint_tcp_port, carrier_endpoint_quic_port = EXCLUDED.carrier_endpoint_quic_port,
    carrier_server_spki_sha256 = EXCLUDED.carrier_server_spki_sha256,
    carrier_server_certificate_chain_pem = EXCLUDED.carrier_server_certificate_chain_pem,
    state = 'registered', ready = false, capacity = EXCLUDED.capacity,
    observation = '{}'::jsonb, last_heartbeat_at = EXCLUDED.last_heartbeat_at,
    drain_deadline = NULL, version = control_tunnel_nodes.version + 1, updated_at = EXCLUDED.last_heartbeat_at
WHERE (NOT EXISTS (SELECT 1 FROM selfhost_installations i WHERE i.node_id=control_tunnel_nodes.id)
   AND NOT EXISTS (SELECT 1 FROM managed_node_authority m WHERE m.node_id=control_tunnel_nodes.id))
   OR EXISTS (SELECT 1 FROM managed_node_authority m JOIN operator_regions r ON r.id=m.region_id
       WHERE m.node_id=control_tunnel_nodes.id AND m.retired_at IS NULL AND r.enabled
         AND (control_tunnel_nodes.state <> 'draining' OR control_tunnel_nodes.drain_deadline IS NULL)
         AND control_tunnel_nodes.state <> 'retired'
         AND EXCLUDED.edge_pool=control_tunnel_nodes.edge_pool
         AND EXCLUDED.endpoint_host=control_tunnel_nodes.endpoint_host
         AND EXCLUDED.endpoint_tcp_port=control_tunnel_nodes.endpoint_tcp_port
         AND EXCLUDED.endpoint_quic_port=control_tunnel_nodes.endpoint_quic_port
         AND EXCLUDED.carrier_endpoint_host=control_tunnel_nodes.carrier_endpoint_host
         AND EXCLUDED.carrier_endpoint_tcp_port=control_tunnel_nodes.carrier_endpoint_tcp_port
         AND EXCLUDED.carrier_endpoint_quic_port=control_tunnel_nodes.carrier_endpoint_quic_port)
   OR EXISTS (SELECT 1 FROM selfhost_installations i
       WHERE i.node_id=control_tunnel_nodes.id AND i.state='active' AND i.capability='tunnel'
         AND i.claim_complete AND octet_length(control_tunnel_nodes.tls_spki_sha256)=32
         AND EXCLUDED.carrier_server_spki_sha256='sha256:'||encode(control_tunnel_nodes.tls_spki_sha256,'hex')
         AND (i.expires_at IS NULL OR i.expires_at>EXCLUDED.last_heartbeat_at)
         AND EXCLUDED.edge_pool=i.id
         AND EXCLUDED.endpoint_host=control_tunnel_nodes.endpoint_host
         AND EXCLUDED.endpoint_tcp_port=control_tunnel_nodes.endpoint_tcp_port
         AND EXCLUDED.endpoint_quic_port=control_tunnel_nodes.endpoint_quic_port
         AND EXCLUDED.carrier_endpoint_host=control_tunnel_nodes.endpoint_host
         AND EXCLUDED.carrier_endpoint_tcp_port=control_tunnel_nodes.endpoint_tcp_port
         AND EXCLUDED.carrier_endpoint_quic_port=control_tunnel_nodes.endpoint_quic_port)
RETURNING *;

-- name: GetControlConnectorAssignment :one
SELECT c.generation, c.edge_pool, c.edge_node_id, (c.revoked_at IS NOT NULL OR e.desired_state = 'revoked') AS revoked
FROM control_connector_generations c
JOIN control_environments e ON e.id = c.environment_id
JOIN user_machines m ON m.id = c.machine_id AND m.environment_id = c.environment_id
WHERE c.environment_id = sqlc.arg(environment_id) AND c.machine_id = sqlc.arg(machine_id)
  AND c.connector_id = sqlc.arg(connector_id)
  AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND e.desired_state = 'active';

-- name: GetControlConnectorGenerationForUpdate :one
SELECT * FROM control_connector_generations
WHERE environment_id = sqlc.arg(environment_id) AND connector_id = sqlc.arg(connector_id)
FOR UPDATE;

-- name: SelectReadyControlTunnelNode :one
SELECT * FROM control_tunnel_nodes
WHERE (edge_pool = sqlc.arg(edge_pool) OR EXISTS (SELECT 1 FROM selfhost_installations i WHERE i.node_id=control_tunnel_nodes.id)) AND state = 'ready' AND ready = true
  AND EXISTS (SELECT 1 FROM tunnels t JOIN tunnel_routes r ON r.tunnel_id=t.id WHERE t.account_id=sqlc.arg(account_id)::text AND t.deleted_at IS NULL AND r.deleted_at IS NULL AND paperboat.node_route_eligible(control_tunnel_nodes.id,t.account_id,r.id))
  AND last_heartbeat_at > sqlc.arg(stale_after)
ORDER BY last_heartbeat_at DESC, id
LIMIT 1;

-- name: SetControlConnectorAdmission :one
UPDATE control_connector_generations
SET edge_node_id = sqlc.arg(edge_node_id), state = 'admitted', admission_jti_hash = sqlc.arg(admission_jti_hash),
    admission_operation_key = sqlc.arg(admission_operation_key), admission_request_hash = sqlc.arg(admission_request_hash),
    admission_credential_ciphertext = sqlc.arg(admission_credential_ciphertext), expires_at = sqlc.arg(expires_at),
    version = version + 1, updated_at = sqlc.arg(updated_at)
WHERE environment_id = sqlc.arg(environment_id) AND connector_id = sqlc.arg(connector_id)
  AND generation = sqlc.arg(generation)
RETURNING *;

-- name: ListControlRoutesForNode :many
SELECT r.id AS route_id, r.desired_revision AS route_revision, r.environment_id, c.connector_id,
       c.generation AS connector_generation, c.edge_node_id, r.kind, r.public_host,
       r.target_host, r.target_port,
       CASE
         WHEN r.applied_revision = r.desired_revision
          AND r.applied_node_id = c.edge_node_id
          AND r.applied_generation = c.generation
         THEN 'ready'::text
         ELSE 'pending'::text
       END AS preview_state,
       CASE
         WHEN r.applied_revision = r.desired_revision
          AND r.applied_node_id = c.edge_node_id
          AND r.applied_generation = c.generation
         THEN ''::text
         ELSE 'route_not_applied'::text
       END AS preview_reason
FROM control_routes r
JOIN control_connector_generations c ON c.environment_id = r.environment_id AND c.connector_id = r.connector_id
JOIN user_machines m ON m.id = c.machine_id AND m.environment_id = c.environment_id
WHERE c.edge_node_id = sqlc.arg(edge_node_id)
  AND r.kind <> 'runtime_https_wss'
  AND r.desired_state IN ('attached','replacing')
  AND c.state IN ('pending','admitted')
  AND m.revoked_at IS NULL AND m.deleted_at IS NULL
ORDER BY r.id;

-- name: ListControlRoutesForEnvironmentAdmission :many
SELECT r.id AS route_id, r.desired_revision AS route_revision, r.kind, r.public_host, r.target_host, r.target_port
FROM control_routes r
WHERE r.environment_id = sqlc.arg(environment_id) AND r.connector_id = sqlc.arg(connector_id)
  AND r.desired_state IN ('attached','replacing')
ORDER BY r.id
LIMIT 128;

-- name: HeartbeatControlTunnelNode :one
UPDATE control_tunnel_nodes
SET state = CASE WHEN sqlc.arg(draining)::boolean THEN 'draining' WHEN state = 'registered' AND sqlc.arg(ready)::boolean THEN 'ready' ELSE state END,
    ready = CASE WHEN state = 'draining' OR sqlc.arg(draining)::boolean THEN false ELSE sqlc.arg(ready) END,
    observation = coalesce(sqlc.arg(observation)::jsonb, '{}'::jsonb),
    capacity_observed_at = CASE WHEN roles @> ARRAY['edge']::text[] THEN sqlc.arg(now) ELSE capacity_observed_at END,
    registry_expires_at = CASE WHEN roles @> ARRAY['edge']::text[] THEN sqlc.arg(now)::timestamptz + interval '1 minute' ELSE registry_expires_at END,
    last_heartbeat_at = sqlc.arg(now), version = version + 1, updated_at = sqlc.arg(now)
WHERE control_tunnel_nodes.id = sqlc.arg(id) AND control_tunnel_nodes.process_epoch = sqlc.arg(process_epoch) AND control_tunnel_nodes.state NOT IN ('offline','retired')
  AND (NOT EXISTS (SELECT 1 FROM managed_node_authority m WHERE m.node_id=control_tunnel_nodes.id)
    OR EXISTS (SELECT 1 FROM managed_node_authority m JOIN operator_regions r ON r.id=m.region_id
      WHERE m.node_id=control_tunnel_nodes.id AND m.retired_at IS NULL AND r.enabled))
RETURNING *;

-- name: GetActiveControlUsageVerificationKey :one
SELECT public_key FROM control_usage_verification_keys
WHERE key_id = sqlc.arg(key_id) AND edge_node_id = sqlc.arg(edge_node_id)
  AND revoked_at IS NULL AND not_before <= sqlc.arg(now) AND expires_at > sqlc.arg(now);

-- name: CreateControlUsageVerificationKey :one
INSERT INTO control_usage_verification_keys (key_id, edge_node_id, public_key, not_before, expires_at)
VALUES (sqlc.arg(key_id), sqlc.arg(edge_node_id), sqlc.arg(public_key), sqlc.arg(not_before), sqlc.arg(expires_at))
ON CONFLICT (key_id) DO NOTHING
RETURNING *;

-- name: GetControlUsageVerificationKey :one
SELECT * FROM control_usage_verification_keys WHERE key_id = $1;

-- name: RevokeControlUsageVerificationKey :execrows
UPDATE control_usage_verification_keys SET revoked_at = coalesce(revoked_at, sqlc.arg(revoked_at))
WHERE key_id = sqlc.arg(key_id) AND revoked_at IS NULL;

-- name: DrainControlTunnelNode :one
UPDATE control_tunnel_nodes
SET state = 'draining', ready = false, drain_deadline = sqlc.arg(drain_deadline),
    version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(expected_version) AND state IN ('registered','ready')
RETURNING *;

-- name: ListStaleControlTunnelNodesForUpdate :many
SELECT * FROM control_tunnel_nodes
WHERE state IN ('registered','ready') AND (last_heartbeat_at IS NULL OR last_heartbeat_at <= sqlc.arg(cutoff))
ORDER BY coalesce(last_heartbeat_at, created_at), id
FOR UPDATE SKIP LOCKED LIMIT sqlc.arg(batch_size);

-- name: MarkControlTunnelNodeOffline :execrows
UPDATE control_tunnel_nodes SET state = 'offline', ready = false, version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(expected_version) AND state IN ('registered','ready');

-- name: FenceControlConnectorsForNode :execrows
UPDATE control_connector_generations
SET edge_node_id = NULL, state = 'pending', generation = generation + 1, admission_jti_hash = NULL,
    admission_operation_key = NULL, admission_request_hash = NULL, admission_credential_ciphertext = NULL,
    expires_at = NULL, version = version + 1, updated_at = sqlc.arg(now)
WHERE edge_node_id = sqlc.arg(edge_node_id) AND state IN ('pending','admitted');

-- name: AdvanceControlRoutesForNodeLoss :execrows
UPDATE control_routes
SET desired_revision = desired_revision + 1, applied_revision = 0, applied_node_id = NULL,
    applied_generation = NULL, version = version + 1, updated_at = sqlc.arg(now)
WHERE applied_node_id = sqlc.arg(edge_node_id) AND desired_state IN ('attached','replacing');

-- name: GetControlRouteForUpdate :one
SELECT * FROM control_routes WHERE id = $1 FOR UPDATE;

-- name: ListTunnelEdgeRouteAssignmentsForNodeV1 :many
-- This is a complete desired snapshot for one edge process.  A missing row is
-- therefore an explicit authorization absence, while a failed query must
-- leave the edge's last-known-good registry untouched.
-- A tunnel on the endpoint engine (PLAN 3a step 4): its route rides on the agent's runtime carrier
-- to this edge. The carrier identity (tunnel, connector, session, generations, config hash) is the
-- runtime carrier's, exactly as the edge admitted it; the route id names the tunnel route. Only an
-- edge process that serves tunnel endpoints is told.
SELECT ('epa_' || x.route_id)::text AS assignment_id,
       x.route_id AS route_id,
       x.generation AS assignment_generation,
       tr.generation AS route_revision,
       t.account_id AS account_id,
       (ra.admission->'binding'->>'tunnel_id')::text AS tunnel_id,
       (ra.admission->'binding'->>'connector_id')::text AS connector_id,
       ra.machine_id AS host_id,
       ra.public_identity_key AS machine_identity_public_key,
       -- Filled by the route snapshot with the shared connector thumbprint of the key (the
       -- carrier's own sha256 thumbprint is another format the edge's registry refuses).
       ''::text AS machine_identity_thumbprint,
       1::bigint AS connector_generation,
       ra.session_id AS connector_session_id,
       ra.worker_generation AS connector_process_generation,
       (ra.admission->'binding'->>'config_generation')::bigint AS config_generation,
       decode(substring(ra.admission->>'config_content_hash' from 8), 'hex') AS config_content_hash,
       viewer_policy.effective_access_mode AS access_mode,
       viewer_policy.viewer_policy_generation AS viewer_policy_generation,
       tr.generation AS route_generation,
       x.edge_node_id AS edge_node_id,
       x.edge_process_epoch AS edge_process_epoch,
       node.failure_domain::text AS edge_failure_domain,
       (CASE tr.protocol WHEN 'tls' THEN 'tunnel_tls' WHEN 'tcp' THEN 'tunnel_tcp' WHEN 'private_tcp' THEN 'tunnel_private_tcp' ELSE 'tunnel_http_wss' END)::text AS kind,
       (CASE WHEN tr.match_type = 'one_label_wildcard' THEN '*.' || tr.wildcard_suffix
             WHEN tr.match_type = 'catch_all' THEN substring(t.stable_endpoint from 9)
             ELSE COALESCE(tr.match_hostname, '') END)::text AS public_host,
       tr.match_type AS match_type,
       tr.match_hostname AS match_hostname,
       tr.wildcard_suffix AS wildcard_suffix,
       tr.path_prefix AS path_prefix,
       tr.priority AS priority,
       tr.protocol AS protocol,
       tr.public_tcp_listener_id AS public_tcp_listener_id,
       tr.public_tcp_port AS public_tcp_port,
       tr.origin_scheme AS origin_scheme,
       tr.preserve_host AS preserve_host,
       tr.host_override AS host_override,
       COALESCE(engine_scoped_domain.hostname, '')::text AS scoped_domain_hostname,
       COALESCE(engine_scoped_domain.match_type, '')::text AS scoped_domain_match_type,
       COALESCE((
         SELECT jsonb_agg(jsonb_build_object('id', d.id, 'hostname', d.hostname, 'match_type', d.match_type, 'generation', d.generation) ORDER BY d.hostname, d.id)
         FROM tunnel_domains AS d
         WHERE d.route_id = tr.id AND d.tunnel_id = tr.tunnel_id AND d.deleted_at IS NULL AND d.ownership_state = 'verified'
           AND ((tr.protocol = 'tls' AND d.certificate_strategy = 'none' AND d.certificate_state = 'not_applicable' AND d.match_type = 'exact')
             OR (tr.protocol <> 'tls' AND d.certificate_state = 'ready'))
           AND d.conflict_state = 'clear'
       ), '[]'::jsonb)::text AS domain_bindings,
       'active' AS state,
       CASE WHEN x.observed_ready THEN 'ready' ELSE 'pending' END AS observed_state,
       tr.desired_state AS route_desired_state
FROM endpoint_edge_assignments AS x
JOIN endpoints AS e ON e.id = x.endpoint_id AND e.kind = 'tunnel' AND e.desired_state = 'active'
JOIN tunnels AS t ON t.id = e.tunnel_id
JOIN endpoint_routes AS er ON er.id = x.route_id
JOIN tunnel_routes AS tr ON tr.id = er.tunnel_route_id AND tr.tunnel_id = t.id
JOIN current_tunnel_route_viewer_policy AS viewer_policy ON viewer_policy.route_id = tr.id
JOIN runtime_carrier_attachments AS ra
  ON ra.machine_id = e.agent_machine_id AND ra.edge_node_id = x.edge_node_id
 AND ra.edge_process_epoch = x.edge_process_epoch AND ra.session_id = x.carrier_session_id
 AND ra.agent_capabilities @> ARRAY['endpoints','tunnel_endpoints']::text[]
JOIN control_tunnel_nodes AS node ON node.id = x.edge_node_id
LEFT JOIN LATERAL (
  SELECT d.hostname, d.match_type
  FROM tunnel_domains d
  WHERE tr.match_type = 'managed'
    AND EXISTS (SELECT 1 FROM selfhost_installations i WHERE i.node_id = node.id AND i.scope_kind <> 'global')
    AND d.route_id = tr.id AND d.tunnel_id = tr.tunnel_id AND d.account_id = t.account_id
    AND d.deleted_at IS NULL AND d.ownership_state = 'verified' AND d.conflict_state = 'clear'
  ORDER BY d.hostname, d.id
  LIMIT 1
) engine_scoped_domain ON true
WHERE x.edge_node_id = sqlc.arg(edge_node_id)
  AND x.edge_process_epoch = sqlc.arg(edge_process_epoch)
  AND x.desired = 'active'
  AND node.tunnel_endpoints_epoch = x.edge_process_epoch
  AND t.desired_state = 'active' AND t.deleted_at IS NULL
  AND (t.expires_at IS NULL OR t.expires_at > sqlc.arg(now))
  AND tr.desired_state = 'active' AND tr.deleted_at IS NULL AND tr.protocol IN ('http', 'tcp', 'tls', 'private_tcp')
  AND paperboat.runtime_carrier_valid(ra.route_id, ra.edge_node_id, ra.edge_process_epoch, false)
  AND node.process_epoch = x.edge_process_epoch
  AND node.state = 'ready' AND node.ready = true
  AND node.registry_expires_at > sqlc.arg(now)
  AND node.roles @> ARRAY['edge']::text[]
  AND node.drain_deadline IS NULL
  AND paperboat.node_route_eligible(node.id, t.account_id, tr.id)
ORDER BY x.route_id, x.generation;

-- name: GetActiveHelperRouteForEnvironment :one
SELECT r.* FROM control_routes r
JOIN control_environments e ON e.id = r.environment_id
JOIN control_connector_generations c ON c.environment_id = r.environment_id AND c.connector_id = r.connector_id
JOIN user_machines m ON m.id = c.machine_id AND m.environment_id = r.environment_id
JOIN control_tunnel_nodes n ON n.id = c.edge_node_id AND n.id = r.applied_node_id
WHERE r.environment_id = sqlc.arg(environment_id)
  AND r.kind = 'runtime_https_wss'
  AND paperboat.runtime_carrier_current(r.id,true)
	AND c.connector_id = 'runtime'
  AND r.desired_state IN ('attached','replacing')
  AND r.applied_revision >= r.desired_revision
  AND r.applied_generation = c.generation
  AND e.desired_state = 'active' AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND c.state = 'admitted'
  AND n.state = 'ready' AND n.ready = true
ORDER BY r.id
LIMIT 1;

-- name: ListActiveHelperRouteHostsForEnvironments :many
-- GetActiveHelperRouteForEnvironment for several environments: each one's first active runtime route (by id).
SELECT DISTINCT ON (r.environment_id) r.environment_id, r.public_host FROM control_routes r
JOIN control_environments e ON e.id = r.environment_id
JOIN control_connector_generations c ON c.environment_id = r.environment_id AND c.connector_id = r.connector_id
JOIN user_machines m ON m.id = c.machine_id AND m.environment_id = r.environment_id
JOIN control_tunnel_nodes n ON n.id = c.edge_node_id AND n.id = r.applied_node_id
WHERE r.environment_id = ANY(sqlc.arg(environment_ids)::text[])
  AND r.kind = 'runtime_https_wss'
  AND paperboat.runtime_carrier_current(r.id,true)
  AND c.connector_id = 'runtime'
  AND r.desired_state IN ('attached','replacing')
  AND r.applied_revision >= r.desired_revision
  AND r.applied_generation = c.generation
  AND e.desired_state = 'active' AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND c.state = 'admitted'
  AND n.state = 'ready' AND n.ready = true
ORDER BY r.environment_id, r.id;

-- name: GetActiveHelperRouteForMachine :one
SELECT r.* FROM control_routes r
JOIN control_environments e ON e.id = r.environment_id
JOIN control_connector_generations c ON c.environment_id = r.environment_id AND c.connector_id = r.connector_id
JOIN user_machines m ON m.id = c.machine_id AND m.environment_id = r.environment_id
JOIN control_tunnel_nodes n ON n.id = c.edge_node_id AND n.id = r.applied_node_id
WHERE m.id = sqlc.arg(machine_id)
  AND m.user_id = sqlc.arg(account_id)
  AND r.kind = 'runtime_https_wss'
  AND paperboat.runtime_carrier_current(r.id,true)
  AND c.connector_id = 'runtime'
  AND r.desired_state IN ('attached','replacing')
  AND r.applied_revision >= r.desired_revision
  AND r.applied_generation = c.generation
  AND e.desired_state = 'active' AND m.revoked_at IS NULL AND m.deleted_at IS NULL
  AND c.state = 'admitted' AND n.state = 'ready' AND n.ready = true
ORDER BY r.id
LIMIT 1;

-- name: GetHelperRouteForEnvironment :one
SELECT * FROM control_routes
WHERE environment_id = sqlc.arg(environment_id)
  AND kind = 'runtime_https_wss'
  AND desired_state IN ('attached','replacing')
ORDER BY id
LIMIT 1;

-- name: ReactivateHelperRouteForEnvironment :one
UPDATE control_routes
SET desired_state = 'attached', desired_revision = desired_revision + 1,
    version = version + 1, updated_at = sqlc.arg(now)
WHERE id = (
  SELECT existing.id FROM control_routes AS existing
  WHERE existing.environment_id = sqlc.arg(environment_id) AND existing.kind = 'runtime_https_wss'
  ORDER BY existing.created_at DESC LIMIT 1
)
RETURNING *;

-- name: CreateControlRoute :one
INSERT INTO control_routes (id, environment_id, connector_id, kind, public_host, target_host, target_port)
VALUES (sqlc.arg(id), sqlc.arg(environment_id), sqlc.arg(connector_id), sqlc.arg(kind), sqlc.arg(public_host), sqlc.arg(target_host), sqlc.arg(target_port))
RETURNING *;

-- name: ReserveControlRouteOperation :one
INSERT INTO control_route_operations (operation_key, operation_type, request_hash, route_id, result_revision)
VALUES (sqlc.arg(operation_key), sqlc.arg(operation_type), sqlc.arg(request_hash), sqlc.arg(route_id), sqlc.narg(result_revision))
ON CONFLICT (operation_key) DO UPDATE SET operation_key = EXCLUDED.operation_key
RETURNING *;

-- name: GetControlRouteOperation :one
SELECT * FROM control_route_operations WHERE operation_key = $1;

-- name: SetControlRouteOperationResult :execrows
UPDATE control_route_operations SET result_revision = sqlc.arg(result_revision), result = sqlc.arg(result)::jsonb
WHERE operation_key = sqlc.arg(operation_key) AND result_revision IS NULL;

-- name: AdvanceControlRouteRevision :one
UPDATE control_routes
SET desired_revision = desired_revision + 1, desired_state = sqlc.arg(desired_state),
    drain_deadline = sqlc.narg(drain_deadline), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND desired_revision = sqlc.arg(expected_revision)
RETURNING *;

-- name: ApplyControlRouteObservation :one
UPDATE control_routes
SET applied_revision = sqlc.arg(route_revision), applied_node_id = sqlc.arg(edge_node_id),
    applied_generation = sqlc.arg(connector_generation), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND desired_revision = sqlc.arg(route_revision)
  AND desired_state IN ('attached','replacing') AND applied_revision <= sqlc.arg(route_revision)
  AND EXISTS (
    SELECT 1 FROM control_connector_generations c
    WHERE c.environment_id = control_routes.environment_id
	  AND c.connector_id = control_routes.connector_id
      AND c.edge_node_id = sqlc.arg(edge_node_id)
      AND c.generation = sqlc.arg(connector_generation)
      AND c.state IN ('pending','admitted')
  )
RETURNING *;

-- name: ListDetachingControlRoutesForNode :many
SELECT id, desired_revision FROM control_routes
WHERE desired_state = 'detaching' AND applied_node_id = sqlc.arg(edge_node_id)
ORDER BY id;

-- name: FinalizeDetachedControlRoute :one
UPDATE control_routes
SET desired_state = 'detached', applied_revision = desired_revision,
    applied_node_id = NULL, applied_generation = NULL,
    version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND desired_state = 'detaching'
  AND desired_revision = sqlc.arg(desired_revision)
  AND applied_node_id = sqlc.arg(edge_node_id)
RETURNING *;

-- name: GetControlUsageCounterForUpdate :one
SELECT * FROM control_usage_counters
WHERE edge_node_id = sqlc.arg(edge_node_id) AND counter_epoch = sqlc.arg(counter_epoch)
  AND environment_id = sqlc.arg(environment_id) AND route_id = sqlc.arg(route_id)
  AND direction = sqlc.arg(direction) AND authority_id = sqlc.arg(authority_id)
FOR UPDATE;

-- name: UpsertControlUsageCounter :one
INSERT INTO control_usage_counters
  (authority_id, edge_node_id, counter_epoch, environment_id, route_id, route_revision, direction, bytes, observed_at)
VALUES
  (sqlc.arg(authority_id), sqlc.arg(edge_node_id), sqlc.arg(counter_epoch), sqlc.arg(environment_id), sqlc.arg(route_id),
   sqlc.arg(route_revision), sqlc.arg(direction), sqlc.arg(bytes), sqlc.arg(observed_at))
ON CONFLICT (edge_node_id, counter_epoch, environment_id, route_id, direction, authority_id) DO UPDATE
SET bytes = greatest(control_usage_counters.bytes, EXCLUDED.bytes),
    route_revision = CASE WHEN EXCLUDED.bytes >= control_usage_counters.bytes THEN EXCLUDED.route_revision ELSE control_usage_counters.route_revision END,
    observed_at = greatest(control_usage_counters.observed_at, EXCLUDED.observed_at)
RETURNING *;

-- name: InsertControlUsageReceipt :one
INSERT INTO control_usage_receipts
  (authority_id, operation_id, edge_node_id, counter_epoch, environment_id, route_id, route_revision, direction,
   observed_bytes, delta_bytes, interval_start, interval_end)
VALUES
  (sqlc.arg(authority_id), sqlc.arg(operation_id), sqlc.arg(edge_node_id), sqlc.arg(counter_epoch), sqlc.arg(environment_id), sqlc.arg(route_id),
   sqlc.arg(route_revision), sqlc.arg(direction), sqlc.arg(observed_bytes), sqlc.arg(delta_bytes),
   sqlc.arg(interval_start), sqlc.arg(interval_end))
ON CONFLICT (operation_id) DO NOTHING
RETURNING *;

-- name: GetControlUsageReceipt :one
SELECT * FROM control_usage_receipts WHERE operation_id = $1;

-- name: AcknowledgeControlUsageReceipt :execrows
UPDATE control_usage_receipts SET acknowledged_at = coalesce(acknowledged_at, sqlc.arg(now))
WHERE operation_id = sqlc.arg(operation_id);

-- name: GetOwnedControlConfigConflictContext :one
SELECT assignment.id AS assignment_id,
       assignment.version AS assignment_version,
       assignment.mode AS assignment_mode,
       assignment.repository_id,
       status.remote_revision,
       status.conflicts
FROM control_environments environment
JOIN control_config_assignments assignment ON assignment.environment_id=environment.id
JOIN control_config_repositories repository ON repository.id=assignment.repository_id
JOIN control_config_sync_statuses status
  ON status.environment_id=environment.id
 AND status.assignment_id=assignment.id
 AND status.repository_id=repository.id
WHERE environment.id=sqlc.arg(environment_id)
  AND environment.owner_user_id=sqlc.arg(owner_user_id)
  AND environment.desired_state='active'
  AND repository.state='active';

-- name: CreateControlConfigConflictResolution :one
INSERT INTO control_config_conflict_resolutions
  (id, environment_id, repository_id, assignment_id, conflict_revision, path, scope,
   action, expected_remote_revision, requested_by_user_id)
VALUES
  (sqlc.arg(id),sqlc.arg(environment_id),sqlc.arg(repository_id),sqlc.arg(assignment_id),
   sqlc.arg(conflict_revision),sqlc.arg(path),sqlc.arg(scope),sqlc.arg(action),
   sqlc.arg(expected_remote_revision),sqlc.arg(requested_by_user_id))
ON CONFLICT (environment_id, conflict_revision, path) DO UPDATE
SET action=EXCLUDED.action,
    expected_remote_revision=EXCLUDED.expected_remote_revision,
    requested_by_user_id=EXCLUDED.requested_by_user_id,
    state='pending',
    landed_revision=NULL,
    applied_at=NULL,
    requested_at=now(),
    updated_at=now()
WHERE control_config_conflict_resolutions.state <> 'applied'
RETURNING *;

-- name: ListPendingControlConfigConflictResolutions :many
SELECT *
FROM control_config_conflict_resolutions
WHERE environment_id=sqlc.arg(environment_id)
  AND repository_id=sqlc.arg(repository_id)
  AND assignment_id=sqlc.arg(assignment_id)
  AND state='pending'
ORDER BY requested_at,id
LIMIT 100;

-- name: ApplyControlConfigConflictResolution :one
UPDATE control_config_conflict_resolutions
SET state='applied', landed_revision=sqlc.arg(landed_revision),
    applied_at=coalesce(applied_at,sqlc.arg(now)), updated_at=sqlc.arg(now)
WHERE id=sqlc.arg(id)
  AND environment_id=sqlc.arg(environment_id)
  AND repository_id=sqlc.arg(repository_id)
  AND assignment_id=sqlc.arg(assignment_id)
  AND (
    state='pending'
    OR (state='applied' AND landed_revision=sqlc.arg(landed_revision))
  )
RETURNING *;
