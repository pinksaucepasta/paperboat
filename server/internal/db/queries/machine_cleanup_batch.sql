-- Set-based machine removal for account deletion (db.DeleteUserMachinesBatchTx): each statement does what its
-- per-machine twin in machine_cleanup.sql / control_plane.sql does, for every machine id in the batch at once.

-- name: BatchE2eeKeys :exec
UPDATE account_e2ee_keys AS k SET revoked_at=coalesce(k.revoked_at,sqlc.arg(now)::timestamptz), revocation_reason=coalesce(k.revocation_reason,'machine_removed'), updated_at=sqlc.arg(now)::timestamptz
WHERE k.user_id=sqlc.arg(user_id)::text AND k.revoked_at IS NULL AND (k.user_machine_id=ANY(sqlc.arg(machine_ids)::text[]) OR k.cli_client_session_id IN (SELECT s.id FROM cli_client_sessions s WHERE s.user_machine_id=ANY(sqlc.arg(machine_ids)::text[]) AND s.user_id=sqlc.arg(user_id)::text));

-- name: BatchCliSessions :exec
UPDATE cli_client_sessions SET state='revoked', revoked_at=coalesce(revoked_at,sqlc.arg(now)::timestamptz), revocation_reason=coalesce(revocation_reason,'machine_removed'), version=version+1
WHERE user_machine_id=ANY(sqlc.arg(machine_ids)::text[]) AND user_id=sqlc.arg(user_id)::text AND state='active';

-- name: BatchCliAccessTokens :exec
UPDATE cli_access_tokens AS t SET revoked_at=coalesce(t.revoked_at,sqlc.arg(now)::timestamptz) FROM cli_client_sessions s
WHERE t.cli_client_session_id=s.id AND s.user_machine_id=ANY(sqlc.arg(machine_ids)::text[]) AND s.user_id=sqlc.arg(user_id)::text AND t.revoked_at IS NULL;

-- name: BatchCliRefreshTokens :exec
UPDATE cli_refresh_tokens AS t SET state='revoked', revoked_at=coalesce(t.revoked_at,sqlc.arg(now)::timestamptz) FROM cli_client_sessions s
WHERE t.cli_client_session_id=s.id AND s.user_machine_id=ANY(sqlc.arg(machine_ids)::text[]) AND s.user_id=sqlc.arg(user_id)::text AND t.state<>'revoked';

-- name: BatchPeerEnrollmentRequests :exec
UPDATE peer_endpoint_enrollment_requests SET state='revoked'
WHERE user_id=sqlc.arg(user_id)::text AND state='pending' AND (endpoint_id=ANY(sqlc.arg(machine_ids)::text[]) OR endpoint_id IN (SELECT s.id FROM cli_client_sessions s WHERE s.user_machine_id=ANY(sqlc.arg(machine_ids)::text[]) AND s.user_id=sqlc.arg(user_id)::text));

-- name: BatchPeerCertificates :exec
UPDATE peer_endpoint_certificates AS c SET revoked_at=coalesce(c.revoked_at,sqlc.arg(now)::timestamptz),
revocation_reason=CASE WHEN c.revoked_at IS NULL THEN 'endpoint_removed' ELSE c.revocation_reason END
WHERE c.user_id=sqlc.arg(user_id)::text AND c.revoked_at IS NULL AND (c.endpoint_id=ANY(sqlc.arg(machine_ids)::text[]) OR c.endpoint_id IN (SELECT s.id FROM cli_client_sessions s WHERE s.user_machine_id=ANY(sqlc.arg(machine_ids)::text[]) AND s.user_id=sqlc.arg(user_id)::text));

-- name: BatchPairings :exec
UPDATE user_machine_pairings SET state='expired', expires_at=least(expires_at,sqlc.arg(now)::timestamptz), installation_config_ciphertext=NULL, installation_config_nonce=NULL,
installation_config_consumed_at=NULL, installation_recovery_operation_key=NULL, updated_at=sqlc.arg(now)::timestamptz
WHERE user_machine_id=ANY(sqlc.arg(machine_ids)::text[]) AND state IN ('pending','approved','consumed');

-- name: BatchPairingInstallations :exec
UPDATE user_machine_pairings SET installation_state='deleted', expires_at=least(expires_at,sqlc.arg(now)::timestamptz), bootstrap_token_hash=NULL, bootstrap_token_ciphertext=NULL, updated_at=sqlc.arg(now)::timestamptz
WHERE user_machine_id=ANY(sqlc.arg(machine_ids)::text[]) AND installation_state IN ('awaiting_bootstrap','awaiting_approval','approved','material_issued','installing','connecting','ready','failed_retryable');

-- name: BatchDiagnosticUploadIntents :exec
UPDATE diagnostic_upload_intents AS u SET state='expired' FROM cli_client_sessions s
WHERE u.cli_client_session_id=s.id AND s.user_machine_id=ANY(sqlc.arg(machine_ids)::text[]) AND s.user_id=sqlc.arg(user_id)::text AND u.state='pending';

-- name: BatchControlSessions :exec
DELETE FROM machine_control_sessions WHERE machine_id=ANY(sqlc.arg(machine_ids)::text[]);

-- name: BatchControlRenewals :exec
DELETE FROM machine_control_renewals WHERE machine_id=ANY(sqlc.arg(machine_ids)::text[]);

-- name: BatchTransferDestinationDefaults :exec
DELETE FROM user_transfer_destination_defaults WHERE user_id=sqlc.arg(user_id)::text AND machine_id=ANY(sqlc.arg(machine_ids)::text[]);

-- name: BatchTerminalTransferDestinations :exec
UPDATE user_machine_terminal_sessions SET transfer_destination_machine_id=NULL, version=version+1, updated_at=sqlc.arg(now)::timestamptz
WHERE transfer_destination_machine_id=ANY(sqlc.arg(machine_ids)::text[]);

-- name: BatchTerminalSessionOperations :exec
DELETE FROM user_machine_terminal_session_operations WHERE user_machine_id=ANY(sqlc.arg(machine_ids)::text[]);

-- name: BatchTerminalSessions :exec
UPDATE user_machine_terminal_sessions SET desired_state='deleted', deleted_at=coalesce(deleted_at,sqlc.arg(now)::timestamptz), version=version+1, updated_at=sqlc.arg(now)::timestamptz
WHERE user_machine_id=ANY(sqlc.arg(machine_ids)::text[]) AND deleted_at IS NULL;

-- name: BatchAvailabilityOperations :exec
DELETE FROM user_machine_availability_operations WHERE user_machine_id=ANY(sqlc.arg(machine_ids)::text[]);

-- name: BatchUpdateObservations :exec
DELETE FROM user_machine_update_observations WHERE user_machine_id=ANY(sqlc.arg(machine_ids)::text[]);

-- name: BatchMaintenanceApprovals :exec
UPDATE user_machine_maintenance_approvals SET status='expired', decided_at=coalesce(decided_at,sqlc.arg(now)::timestamptz), updated_at=sqlc.arg(now)::timestamptz
WHERE user_machine_id=ANY(sqlc.arg(machine_ids)::text[]) AND status IN ('pending','approved');

-- name: BatchSshHostKeys :exec
DELETE FROM machine_ssh_host_keys WHERE user_machine_id=ANY(sqlc.arg(machine_ids)::text[]);

-- name: BatchSshHostKeySets :exec
DELETE FROM machine_ssh_host_key_sets WHERE user_machine_id=ANY(sqlc.arg(machine_ids)::text[]);

-- name: BatchSshHostKeyOwners :exec
DELETE FROM machine_ssh_host_key_owners WHERE user_machine_id=ANY(sqlc.arg(machine_ids)::text[]);

-- name: BatchSshTargets :exec
DELETE FROM machine_ssh_targets WHERE user_machine_id=ANY(sqlc.arg(machine_ids)::text[]);

-- name: BatchRevokeEnvEnvironments :exec
UPDATE control_environments SET desired_state='revoked', desired_version=desired_version+1, revoked_at=coalesce(revoked_at,sqlc.arg(now)::timestamptz), updated_at=sqlc.arg(now)::timestamptz
WHERE id=ANY(sqlc.arg(environment_ids)::text[]) AND desired_state<>'revoked';

-- name: BatchRevokeEnvHelpers :exec
UPDATE control_helpers SET state='revoked', revoked_at=coalesce(revoked_at,sqlc.arg(now)::timestamptz), updated_at=sqlc.arg(now)::timestamptz
WHERE environment_id=ANY(sqlc.arg(environment_ids)::text[]) AND state IN ('pending','active') AND revoked_at IS NULL;

-- name: BatchRevokeEnvHelperEnrollments :exec
UPDATE control_helper_enrollments SET state='revoked', revoked_at=coalesce(revoked_at,sqlc.arg(now)::timestamptz)
WHERE environment_id=ANY(sqlc.arg(environment_ids)::text[]) AND state='pending' AND revoked_at IS NULL;

-- name: BatchRevokeEnvConnectors :exec
UPDATE control_connector_generations SET state='revoked', revoked_at=coalesce(revoked_at,sqlc.arg(now)::timestamptz), version=version+1, updated_at=sqlc.arg(now)::timestamptz
WHERE environment_id=ANY(sqlc.arg(environment_ids)::text[]) AND state IN ('pending','admitted','draining') AND revoked_at IS NULL;

-- name: BatchRevokeEnvRoutes :exec
UPDATE control_routes SET desired_state=CASE WHEN applied_node_id IS NULL THEN 'detached' ELSE 'detaching' END,
desired_revision=desired_revision+1, version=version+1, updated_at=sqlc.arg(now)::timestamptz
WHERE environment_id=ANY(sqlc.arg(environment_ids)::text[]) AND desired_state NOT IN ('detaching','detached');

-- name: BatchRevokeEnvConfigCredentials :exec
UPDATE control_config_credentials SET revoked_at=coalesce(revoked_at,sqlc.arg(now)::timestamptz) WHERE environment_id=ANY(sqlc.arg(environment_ids)::text[]) AND revoked_at IS NULL;

-- name: BatchRevokeEnvConfigRepositoryAccess :exec
UPDATE control_config_repository_access_operations SET state='revoked', revoked_at=coalesce(revoked_at,sqlc.arg(now)::timestamptz), updated_at=sqlc.arg(now)::timestamptz
WHERE environment_id=ANY(sqlc.arg(environment_ids)::text[]) AND revoked_at IS NULL AND state IN ('pending','issued','uncertain');

-- name: BatchRevokeEnvConfigRepositoryLeases :exec
UPDATE control_config_repository_lease_authority SET revoked_at=sqlc.arg(now)::timestamptz, version=version+1, updated_at=sqlc.arg(now)::timestamptz
WHERE environment_id=ANY(sqlc.arg(environment_ids)::text[]) AND lease_id IS NOT NULL AND revoked_at IS NULL;

-- name: BatchDeleteUserMachines :execrows
UPDATE user_machines SET state='deleted', online=false, seat_state='released', deleted_at=now(), updated_at=now(), version=version+1
WHERE id=ANY(sqlc.arg(machine_ids)::text[]) AND user_id=sqlc.arg(user_id)::text AND deleted_at IS NULL;

-- name: BatchRevokeUserMachineAccessSessions :exec
UPDATE user_machine_access_sessions AS a SET state='revoked', revoked_at=coalesce(a.revoked_at,now()), revocation_reason='user_machine_deleted', updated_at=now(),
helper_revoked_at=CASE WHEN a.credential_kind='control_plane_signed' THEN now() END
WHERE a.user_machine_id=ANY(sqlc.arg(machine_ids)::text[]) AND a.state='active' AND (a.expires_at>now() OR a.credential_kind<>'control_plane_signed');
