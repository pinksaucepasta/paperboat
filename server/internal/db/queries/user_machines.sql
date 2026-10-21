-- name: CreateUserMachinePairing :one
INSERT INTO user_machine_pairings (
  id, verifier_hash, user_code, requested_alias, requested_description, platform, architecture,
  workspace_root, runtime_versions, public_identity_key, ssh_user, ssh_port,
  can_reuse_runtime_identity, keep_awake, installation_state, generation, expires_at, replaces_machine_id
) VALUES (
  sqlc.arg(id), sqlc.arg(verifier_hash), sqlc.arg(user_code), sqlc.arg(requested_alias), sqlc.arg(requested_description),
  sqlc.arg(platform), sqlc.arg(architecture), sqlc.arg(workspace_root), sqlc.arg(runtime_versions), sqlc.arg(public_identity_key),
  sqlc.narg(ssh_user), sqlc.narg(ssh_port), sqlc.arg(can_reuse_runtime_identity), sqlc.arg(keep_awake),
  'awaiting_approval', 1, sqlc.arg(expires_at), sqlc.narg(replaces_machine_id)
) RETURNING *;

-- name: CreateAuthenticatedMachineInstallationPairing :one
INSERT INTO user_machine_pairings (
  id, verifier_hash, user_code, requested_alias, requested_description, platform, architecture,
  workspace_root, runtime_versions, public_identity_key, ssh_user, ssh_port,
  can_reuse_runtime_identity, keep_awake, state, installation_state, generation, approved_by_user_id, user_machine_id,
  authenticated_setup_cli_session_id, authenticated_setup_operation_id,
  authenticated_setup_generation,
  approved_at, expires_at
) VALUES (
  sqlc.arg(id), sqlc.arg(verifier_hash), sqlc.arg(user_code), sqlc.arg(requested_alias), sqlc.arg(requested_description),
  sqlc.arg(platform), sqlc.arg(architecture), sqlc.arg(workspace_root), sqlc.arg(runtime_versions),
  sqlc.arg(public_identity_key), sqlc.narg(ssh_user), sqlc.narg(ssh_port),
  sqlc.arg(can_reuse_runtime_identity), sqlc.arg(keep_awake), 'approved', 'approved', 1, sqlc.arg(user_id), sqlc.arg(user_machine_id),
  sqlc.arg(cli_client_session_id), sqlc.arg(operation_id), sqlc.arg(installation_generation),
  now(), sqlc.arg(expires_at)
)
ON CONFLICT (authenticated_setup_cli_session_id, authenticated_setup_operation_id)
  WHERE authenticated_setup_cli_session_id IS NOT NULL
DO UPDATE SET authenticated_setup_operation_id = excluded.authenticated_setup_operation_id
RETURNING *;

-- name: UpsertPairingMachineSSHTarget :exec
INSERT INTO machine_ssh_targets
  (user_machine_id, machine_generation, os_user, target_port, created_at, updated_at)
VALUES
  (sqlc.arg(user_machine_id), sqlc.arg(machine_generation), sqlc.arg(os_user), sqlc.arg(target_port), now(), now())
ON CONFLICT (user_machine_id) DO UPDATE SET
  machine_generation = excluded.machine_generation,
  os_user = excluded.os_user,
  target_port = excluded.target_port,
  reconciliation_version = machine_ssh_targets.reconciliation_version +
    CASE WHEN machine_ssh_targets.machine_generation IS DISTINCT FROM excluded.machine_generation
           OR machine_ssh_targets.os_user IS DISTINCT FROM excluded.os_user
           OR machine_ssh_targets.target_port IS DISTINCT FROM excluded.target_port THEN 1 ELSE 0 END,
  updated_at = CASE WHEN machine_ssh_targets.machine_generation IS DISTINCT FROM excluded.machine_generation
                      OR machine_ssh_targets.os_user IS DISTINCT FROM excluded.os_user
                      OR machine_ssh_targets.target_port IS DISTINCT FROM excluded.target_port
                    THEN now() ELSE machine_ssh_targets.updated_at END;

-- name: CreateBootstrapPairing :one
INSERT INTO user_machine_pairings
(id,bootstrap_owner_account,operation_id,idempotency_key,bootstrap_token_hash,bootstrap_token_ciphertext,state,installation_state,generation,expires_at,workspace_team_id)
VALUES (sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(operation_id),sqlc.arg(idempotency_key),sqlc.arg(bootstrap_token_hash),sqlc.arg(bootstrap_token_ciphertext),'awaiting_bootstrap','awaiting_bootstrap',1,sqlc.arg(expires_at),sqlc.narg(workspace_team_id))
ON CONFLICT (bootstrap_owner_account,idempotency_key) DO UPDATE SET idempotency_key=excluded.idempotency_key
RETURNING *;

-- name: GetBootstrapPairingForUser :one
SELECT * FROM user_machine_pairings
WHERE id=sqlc.arg(id) AND bootstrap_owner_account=sqlc.arg(user_id);

-- name: GetBootstrapPairingForTokenUpdate :one
SELECT * FROM user_machine_pairings WHERE bootstrap_token_hash=sqlc.arg(bootstrap_token_hash) FOR UPDATE;

-- name: ClaimBootstrapPairing :one
UPDATE user_machine_pairings
SET state='pending',installation_state='awaiting_approval',
    verifier_hash=sqlc.arg(verifier_hash),user_code=sqlc.arg(user_code),
    requested_alias=sqlc.arg(requested_alias),requested_description=sqlc.arg(requested_description),
    platform=sqlc.arg(platform),architecture=sqlc.arg(architecture),workspace_root=sqlc.arg(workspace_root),
    runtime_versions=sqlc.arg(runtime_versions),public_identity_key=sqlc.arg(public_identity_key),
    ssh_user=sqlc.arg(ssh_user),ssh_port=sqlc.arg(ssh_port),
    can_reuse_runtime_identity=sqlc.arg(can_reuse_runtime_identity),keep_awake=sqlc.arg(keep_awake),replaces_machine_id=sqlc.narg(replaces_machine_id),updated_at=now()
WHERE id=sqlc.arg(id) AND state='awaiting_bootstrap' AND installation_state='awaiting_bootstrap' AND expires_at>now()
RETURNING *;

-- name: FailBootstrapPairingApproval :execrows
UPDATE user_machine_pairings
SET state='expired',installation_state='failed_retryable',expires_at=least(expires_at,sqlc.arg(failed_at)),
    installation_config_ciphertext=NULL,installation_config_nonce=NULL,installation_config_consumed_at=NULL,
    installation_recovery_operation_key=NULL,updated_at=sqlc.arg(failed_at)
WHERE id=sqlc.arg(pairing_id) AND bootstrap_owner_account=sqlc.arg(user_id)
  AND state='pending' AND installation_state='awaiting_approval';

-- name: CancelBootstrapPairing :execrows
UPDATE user_machine_pairings
SET installation_state='cancelled',state='expired',cancelled_at=now(),expires_at=least(expires_at,now()),updated_at=now()
WHERE id=sqlc.arg(id) AND bootstrap_owner_account=sqlc.arg(user_id)
  AND installation_state IN ('awaiting_bootstrap','awaiting_approval','failed_retryable');

-- name: ExpireBootstrapPairing :execrows
UPDATE user_machine_pairings SET installation_state='expired',state='expired',updated_at=now()
WHERE id=sqlc.arg(id) AND bootstrap_owner_account=sqlc.arg(user_id)
  AND installation_state IN ('awaiting_bootstrap','awaiting_approval') AND expires_at<=now();

-- name: RetryBootstrapPairing :one
UPDATE user_machine_pairings
SET state='awaiting_bootstrap',installation_state='awaiting_bootstrap',generation=generation+1,
    bootstrap_token_hash=sqlc.arg(bootstrap_token_hash),bootstrap_token_ciphertext=sqlc.arg(bootstrap_token_ciphertext),
    verifier_hash=NULL,user_code=NULL,requested_alias=NULL,requested_description='',platform=NULL,architecture=NULL,
    workspace_root=NULL,public_identity_key=NULL,runtime_versions='{}'::jsonb,ssh_user=NULL,ssh_port=NULL,
    approved_by_user_id=NULL,approved_at=NULL,denied_at=NULL,
    installation_config_ciphertext=NULL,installation_config_nonce=NULL,installation_config_consumed_at=NULL,
    installation_recovery_operation_key=NULL,expires_at=sqlc.arg(expires_at),cancelled_at=NULL,updated_at=now()
WHERE id=sqlc.arg(id) AND bootstrap_owner_account=sqlc.arg(user_id)
  AND authenticated_setup_cli_session_id IS NULL
  AND installation_state IN ('cancelled','expired','denied','failed_retryable')
RETURNING *;

-- name: FailPairingInstallationForHelper :one
WITH target AS MATERIALIZED (
  SELECT p.id AS pairing_id,m.id AS machine_id,h.id AS helper_id
  FROM user_machine_pairings p
  JOIN user_machines m ON m.id=p.user_machine_id
  JOIN control_helpers h ON h.environment_id=m.environment_id
  JOIN control_helper_enrollments he ON he.helper_id=h.id AND he.environment_id=h.environment_id
  WHERE p.id=sqlc.arg(id) AND m.environment_id=sqlc.arg(environment_id)
    AND h.id=sqlc.arg(helper_id) AND he.id=sqlc.arg(helper_enrollment_id)
    AND he.state IN ('pending','consumed') AND he.revoked_at IS NULL
    AND h.state IN ('pending','active') AND h.revoked_at IS NULL
    AND p.installation_state IN ('installing','connecting')
  FOR UPDATE OF p,m,h,he
), failed AS (
  UPDATE user_machine_pairings p SET installation_state='failed_retryable',updated_at=now()
  FROM target t WHERE p.id=t.pairing_id RETURNING p.id
), released AS (
  UPDATE user_machines m SET seat_state='released',online=false,updated_at=now(),version=version+1
  FROM target t WHERE m.id=t.machine_id AND m.seat_state='occupied' RETURNING m.id
), revoked_helper AS (
  UPDATE control_helpers h SET state='revoked',revoked_at=now(),updated_at=now()
  FROM target t WHERE h.id=t.helper_id RETURNING h.id
), revoked_grants AS (
  UPDATE control_helper_enrollments he SET state='revoked',revoked_at=coalesce(he.revoked_at,now())
  FROM target t WHERE he.helper_id=t.helper_id AND he.state IN ('pending','consumed') AND he.revoked_at IS NULL
  RETURNING he.id
)
SELECT (SELECT count(*) FROM failed)::integer AS failed_count,
       (SELECT count(*) FROM released)::integer AS released_count,
       (SELECT count(*) FROM revoked_helper)::integer AS revoked_helper_count,
       (SELECT count(*) FROM revoked_grants)::integer AS revoked_grant_count;

-- name: GetUserMachineEntitlementForUpdate :one
SELECT * FROM user_machine_entitlements
WHERE user_id = sqlc.arg(user_id) FOR UPDATE;

-- name: GetUserMachineEntitlement :one
SELECT * FROM user_machine_entitlements
WHERE user_id = sqlc.arg(user_id);

-- name: GetUserMachineBandwidthUsage :one
SELECT
  coalesce(sum(p.included_bytes), 0)::bigint AS included_bytes,
  coalesce(sum(p.consumed_included_bytes), 0)::bigint AS consumed_included_bytes,
  coalesce(sum(p.consumed_topup_bytes), 0)::bigint AS consumed_topup_bytes,
  coalesce((SELECT sum(t.remaining_bytes) FROM user_machine_bandwidth_topups t
    WHERE t.user_id = sqlc.arg(user_id) AND t.state = 'active'
      AND t.remaining_bytes > 0 AND (t.expires_at IS NULL OR t.expires_at > now())), 0)::bigint AS paid_topup_remaining_bytes
FROM user_machine_bandwidth_periods p
JOIN user_machines m ON m.id = p.user_machine_id
JOIN user_machine_entitlements e ON e.user_id = m.user_id
WHERE m.user_id = sqlc.arg(user_id)
  AND p.period_start = e.current_period_start
  AND p.period_end = e.current_period_end;

-- name: UserMachineEntitlementIsActive :one
SELECT EXISTS (
  SELECT 1 FROM user_machine_entitlements
  WHERE user_id = sqlc.arg(user_id)
    AND state IN ('active', 'trialing')
    AND current_period_start <= now()
    AND current_period_end > now()
);

-- name: GetActiveUserMachineSeatQuantity :one
SELECT seat_quantity FROM user_machine_entitlements
WHERE user_id = sqlc.arg(user_id)
  AND state IN ('active', 'trialing')
  AND current_period_start <= now()
  AND current_period_end > now();

-- name: UpsertUserMachineEntitlement :exec
INSERT INTO user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end)
VALUES (sqlc.arg(id),sqlc.arg(user_id),sqlc.arg(provider_subscription_id),sqlc.arg(product_code),sqlc.arg(state),sqlc.arg(seat_quantity),sqlc.arg(allowance_bytes),sqlc.arg(current_period_start),sqlc.arg(current_period_end))
ON CONFLICT (user_id) DO UPDATE SET provider_subscription_id=EXCLUDED.provider_subscription_id,product_code=EXCLUDED.product_code,state=EXCLUDED.state,seat_quantity=EXCLUDED.seat_quantity,allowance_bytes=EXCLUDED.allowance_bytes,current_period_start=EXCLUDED.current_period_start,current_period_end=EXCLUDED.current_period_end,updated_at=now();

-- name: UpdateUserMachineEntitlementState :execrows
UPDATE user_machine_entitlements
SET state = sqlc.arg(state), updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND provider_subscription_id = sqlc.arg(provider_subscription_id);

-- name: CountOccupiedUserMachineSeats :one
SELECT count(*)::integer FROM user_machines
WHERE user_id = sqlc.arg(user_id) AND seat_state = 'occupied' AND deleted_at IS NULL;

-- name: ListBillableUserMachineIDs :many
SELECT id FROM user_machines
WHERE user_id = sqlc.arg(user_id)
  AND seat_state = 'occupied'
  AND deleted_at IS NULL
  AND state IN ('pending', 'online', 'offline');

-- name: RevokeUserMachinesForEntitlement :many
WITH revoked AS (
  UPDATE user_machines
  SET state = 'revoked', online = false, seat_state = 'released', revoked_at = now(), updated_at = now(), version = version + 1
  WHERE user_id = sqlc.arg(user_id)
    AND seat_state = 'occupied'
    AND deleted_at IS NULL
    AND state IN ('pending', 'online', 'offline')
  RETURNING id
), expired_installations AS (
  UPDATE user_machine_pairings pairing
  SET state = 'expired', expires_at = least(pairing.expires_at, now()), updated_at = now()
  FROM revoked
  WHERE pairing.user_machine_id = revoked.id
    AND pairing.authenticated_setup_cli_session_id IS NOT NULL
    AND pairing.state IN ('approved', 'consumed')
  RETURNING pairing.authenticated_setup_helper_enrollment_id
), revoked_grants AS (
  UPDATE control_helper_enrollments enrollment
  SET state = 'revoked', revoked_at = coalesce(enrollment.revoked_at, now())
  FROM expired_installations
  WHERE enrollment.id = expired_installations.authenticated_setup_helper_enrollment_id
    AND enrollment.state = 'pending'
    AND enrollment.revoked_at IS NULL
)
SELECT id FROM revoked;

-- name: RevokeUserMachinesOverSeatLimit :many
WITH excess AS (
  SELECT candidate.id
  FROM user_machines AS candidate
  WHERE candidate.user_id = sqlc.arg(user_id)
    AND seat_state = 'occupied'
    AND deleted_at IS NULL
    AND state IN ('pending', 'online', 'offline')
  ORDER BY coalesce(enrolled_at, created_at) ASC, created_at ASC, id ASC
  OFFSET sqlc.arg(seat_quantity)
)
UPDATE user_machines AS machine
SET state = 'revoked', online = false, seat_state = 'released', revoked_at = now(), updated_at = now(), version = version + 1
FROM excess
WHERE machine.id = excess.id
RETURNING machine.id, machine.environment_id;

-- name: ListRevokedUserMachineEnvironmentsForUser :many
SELECT id, environment_id FROM user_machines
WHERE user_id = sqlc.arg(user_id) AND state = 'revoked' AND deleted_at IS NULL
ORDER BY id;

-- name: GetUserMachinePairingForVerifier :one
SELECT * FROM user_machine_pairings
WHERE verifier_hash = sqlc.arg(verifier_hash) FOR UPDATE;

-- name: GetUserMachinePairingForCode :one
SELECT * FROM user_machine_pairings
WHERE user_code = sqlc.arg(user_code) FOR UPDATE;

-- name: GetUserMachinePairingByID :one
SELECT * FROM user_machine_pairings WHERE id = sqlc.arg(id);

-- name: ExpireUserMachinePairing :execrows
UPDATE user_machine_pairings SET state = 'expired', installation_state=CASE WHEN installation_state='awaiting_approval' THEN 'expired' ELSE installation_state END, updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'pending' AND expires_at <= now();

-- name: ExpireAuthenticatedMachineInstallationPairingsForMachine :execrows
UPDATE user_machine_pairings
SET state = 'expired', expires_at = least(expires_at, sqlc.arg(now)), updated_at = sqlc.arg(now)
WHERE user_machine_id = sqlc.arg(user_machine_id)
  AND authenticated_setup_cli_session_id IS NOT NULL
  AND state IN ('approved', 'consumed');

-- name: ExpireAuthenticatedMachineInstallationPairingsForCLISession :one
WITH expired AS (
  UPDATE user_machine_pairings
  SET state = 'expired', expires_at = least(expires_at, sqlc.arg(now)), updated_at = sqlc.arg(now)
  WHERE authenticated_setup_cli_session_id = sqlc.arg(cli_client_session_id)
    AND state IN ('approved', 'consumed')
  RETURNING authenticated_setup_helper_enrollment_id
), revoked_grants AS (
  UPDATE control_helper_enrollments enrollment
  SET state = 'revoked', revoked_at = coalesce(enrollment.revoked_at, sqlc.arg(now))
  FROM expired
  WHERE enrollment.id = expired.authenticated_setup_helper_enrollment_id
    AND enrollment.state = 'pending'
    AND enrollment.revoked_at IS NULL
)
SELECT count(*)::bigint FROM expired;

-- name: CreateEnrolledMachine :one
INSERT INTO user_machines (
  id, user_id, environment_id, description, alias, platform, architecture, workspace_root,
  state, seat_state, runtime_versions, configured_capabilities, public_identity_key, enrolled_at
) VALUES (
  sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(environment_id), sqlc.arg(description), sqlc.arg(alias),
  sqlc.arg(platform), sqlc.arg(architecture), sqlc.arg(workspace_root),
  'offline', 'released', sqlc.arg(runtime_versions), sqlc.arg(configured_capabilities), sqlc.arg(public_identity_key), now()
) RETURNING *;

-- name: OptInUserMachineKeepAwake :one
UPDATE user_machines
SET availability_mode = 'keep_awake',
    availability_desired_version = availability_desired_version + 1,
    availability_status = 'pending', availability_error_code = NULL,
    updated_at = now(), version = version + 1
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND deleted_at IS NULL
  AND availability_mode <> 'keep_awake'
RETURNING *;

-- name: UserMachineAliasExists :one
SELECT EXISTS (
  SELECT 1 FROM user_machines
  WHERE user_id = sqlc.arg(user_id) AND owner_team_id IS NULL AND lower(alias) = lower(sqlc.arg(alias)) AND deleted_at IS NULL
);

-- name: LockUserMachineAliases :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(user_id), 0));

-- name: GetUserMachineByPublicIdentityForUpdate :one
SELECT * FROM user_machines
WHERE public_identity_key = sqlc.arg(public_identity_key) AND deleted_at IS NULL
FOR UPDATE;

-- name: GetCanonicalMachineForEnvironment :one
SELECT * FROM user_machines
WHERE environment_id = sqlc.arg(environment_id) AND deleted_at IS NULL;

-- name: GetActiveUserMachineForControl :one
SELECT * FROM user_machines
WHERE id = sqlc.arg(id) AND deleted_at IS NULL AND revoked_at IS NULL
  AND public_identity_key IS NOT NULL AND seat_state = 'occupied';

-- name: GetOwnedActiveUserMachineForControl :one
SELECT * FROM user_machines
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)
  AND deleted_at IS NULL AND revoked_at IS NULL AND public_identity_key IS NOT NULL AND seat_state = 'occupied';

-- name: CreateMachineControlRenewal :one
INSERT INTO machine_control_renewals (
  operation_id, machine_id, installation_generation, credential_jti, issued_at, expires_at
) VALUES (
  sqlc.arg(operation_id), sqlc.arg(machine_id), sqlc.arg(installation_generation),
  sqlc.arg(credential_jti), sqlc.arg(issued_at), sqlc.arg(expires_at)
) ON CONFLICT (operation_id) DO UPDATE SET operation_id = EXCLUDED.operation_id
RETURNING *;

-- name: GetCurrentMachineControlSession :one
SELECT * FROM machine_control_sessions
WHERE machine_id = sqlc.arg(machine_id)
  AND installation_generation = sqlc.arg(installation_generation)
  AND credential_jti = sqlc.arg(credential_jti)
  AND expires_at > sqlc.arg(now);

-- name: GetReplayableMachineControlRenewal :one
-- A committed renewal is replayable by the credential it replaced: the same
-- operation, still the machine's current session, replacing exactly the
-- presented credential, and not yet expired.
SELECT x.* FROM machine_control_renewals x
JOIN machine_control_sessions s
  ON s.machine_id = x.machine_id AND s.operation_id = x.operation_id
 AND s.credential_jti = x.credential_jti AND s.session_generation = x.session_generation
JOIN machine_control_renewals y
  ON y.machine_id = x.machine_id AND y.installation_generation = x.installation_generation
 AND y.credential_jti = sqlc.arg(previous_credential_jti)
 AND y.session_generation = x.session_generation - 1 AND y.superseded_at IS NOT NULL
WHERE x.operation_id = sqlc.arg(operation_id) AND x.machine_id = sqlc.arg(machine_id)
  AND x.installation_generation = sqlc.arg(installation_generation)
  AND x.superseded_at IS NULL AND x.expires_at > sqlc.arg(now);

-- name: GetMachineControlSessionForUpdate :one
SELECT * FROM machine_control_sessions
WHERE machine_id = sqlc.arg(machine_id)
FOR UPDATE;

-- name: CreateMachineControlSession :one
INSERT INTO machine_control_sessions (
  machine_id, installation_generation, session_generation, operation_id,
  credential_jti, issued_at, expires_at
) VALUES (
  sqlc.arg(machine_id), sqlc.arg(installation_generation), 1, sqlc.arg(operation_id),
  sqlc.arg(credential_jti), sqlc.arg(issued_at), sqlc.arg(expires_at)
)
RETURNING *;

-- name: RotateMachineControlSession :one
UPDATE machine_control_sessions
SET installation_generation = sqlc.arg(installation_generation),
    session_generation = session_generation + 1,
    operation_id = sqlc.arg(operation_id),
    credential_jti = sqlc.arg(credential_jti),
    issued_at = sqlc.arg(issued_at),
    expires_at = sqlc.arg(expires_at),
    updated_at = now()
WHERE machine_id = sqlc.arg(machine_id)
  AND session_generation = sqlc.arg(expected_session_generation)
  AND credential_jti = sqlc.arg(expected_credential_jti)
RETURNING *;

-- name: BindMachineControlRenewalSession :one
UPDATE machine_control_renewals
SET session_generation = sqlc.arg(session_generation)
WHERE operation_id = sqlc.arg(operation_id)
  AND machine_id = sqlc.arg(machine_id)
  AND installation_generation = sqlc.arg(installation_generation)
  AND credential_jti = sqlc.arg(credential_jti)
  AND session_generation IS NULL
  AND superseded_at IS NULL
RETURNING *;

-- name: MarkMachineControlRenewalSuperseded :execrows
UPDATE machine_control_renewals
SET superseded_at = sqlc.arg(now)
WHERE operation_id = sqlc.arg(operation_id)
  AND machine_id = sqlc.arg(machine_id)
  AND session_generation = sqlc.arg(session_generation)
  AND superseded_at IS NULL;

-- name: GetMachineControlRenewalForUpdate :one
SELECT * FROM machine_control_renewals
WHERE operation_id = sqlc.arg(operation_id)
FOR UPDATE;

-- name: RotateMachineControlRenewal :one
UPDATE machine_control_renewals
SET credential_jti = sqlc.arg(credential_jti),
    issued_at = sqlc.arg(issued_at),
    expires_at = sqlc.arg(expires_at),
    session_generation = NULL
WHERE operation_id = sqlc.arg(operation_id)
  AND machine_id = sqlc.arg(machine_id)
  AND installation_generation = sqlc.arg(installation_generation)
RETURNING *;

-- name: DeleteExpiredMachineControlRenewals :execrows
DELETE FROM machine_control_renewals renewals
WHERE renewals.expires_at < sqlc.arg(cutoff)
  AND NOT EXISTS (
    SELECT 1 FROM machine_control_sessions current
    WHERE current.operation_id = renewals.operation_id
  );

-- name: BindCanonicalMachineIdentity :one
UPDATE user_machines
SET public_identity_key = sqlc.arg(public_identity_key),
    installation_generation = installation_generation +
      CASE WHEN public_identity_key IS NOT NULL AND public_identity_key <> sqlc.arg(public_identity_key) THEN 1 ELSE 0 END,
    enrolled_at = coalesce(enrolled_at, sqlc.arg(now)), updated_at = sqlc.arg(now),
    version = version + CASE WHEN public_identity_key IS DISTINCT FROM sqlc.arg(public_identity_key) THEN 1 ELSE 0 END
WHERE environment_id = sqlc.arg(environment_id) AND deleted_at IS NULL
RETURNING *;

-- name: UpdateUserMachineInstallation :one
UPDATE user_machines
SET alias = sqlc.arg(alias), description = sqlc.arg(description), workspace_root = sqlc.arg(workspace_root),
 runtime_versions = sqlc.arg(runtime_versions),
 updated_at = CASE WHEN alias IS DISTINCT FROM sqlc.arg(alias) OR description IS DISTINCT FROM sqlc.arg(description) OR workspace_root IS DISTINCT FROM sqlc.arg(workspace_root) OR runtime_versions IS DISTINCT FROM sqlc.arg(runtime_versions) THEN now() ELSE updated_at END,
 version = version + CASE WHEN alias IS DISTINCT FROM sqlc.arg(alias) OR description IS DISTINCT FROM sqlc.arg(description) OR workspace_root IS DISTINCT FROM sqlc.arg(workspace_root) OR runtime_versions IS DISTINCT FROM sqlc.arg(runtime_versions) THEN 1 ELSE 0 END
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND machine_management_allowed(sqlc.arg(user_id),id) AND deleted_at IS NULL
RETURNING *;

-- name: UpdateUserMachineMetadata :one
UPDATE user_machines
SET alias = sqlc.arg(alias), description = sqlc.arg(description), runtime_versions = sqlc.arg(runtime_versions),
 updated_at = CASE WHEN alias IS DISTINCT FROM sqlc.arg(alias) OR description IS DISTINCT FROM sqlc.arg(description) OR runtime_versions IS DISTINCT FROM sqlc.arg(runtime_versions) THEN now() ELSE updated_at END,
 version = version + CASE WHEN alias IS DISTINCT FROM sqlc.arg(alias) OR description IS DISTINCT FROM sqlc.arg(description) OR runtime_versions IS DISTINCT FROM sqlc.arg(runtime_versions) THEN 1 ELSE 0 END
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND machine_management_allowed(sqlc.arg(user_id),id) AND deleted_at IS NULL
RETURNING *;

-- name: OccupyUserMachineSeat :execrows
UPDATE user_machines
SET seat_state = 'occupied', updated_at = now(), version = version + 1
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)
  AND seat_state = 'released' AND deleted_at IS NULL
  AND state IN ('pending','offline','online');

-- name: ApproveUserMachinePairing :execrows
UPDATE user_machine_pairings
SET state = 'approved', installation_state='approved', approved_by_user_id = sqlc.arg(user_id),
    user_machine_id = sqlc.arg(user_machine_id), approved_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'pending' AND expires_at > now();

-- name: DenyUserMachinePairing :execrows
UPDATE user_machine_pairings
SET state = 'denied', installation_state='denied', approved_by_user_id = sqlc.arg(user_id), denied_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'pending' AND expires_at > now();

-- name: ListUserMachinesForUser :many
SELECT * FROM user_machines
WHERE (machine_management_allowed(sqlc.arg(user_id),id) OR EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap WHERE machine_capability_allowed(sqlc.arg(user_id),id,cap))) AND deleted_at IS NULL
ORDER BY lower(alias), id
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountUserMachinesForUser :one
SELECT count(*)::integer FROM user_machines
WHERE (machine_management_allowed(sqlc.arg(user_id),id) OR EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap WHERE machine_capability_allowed(sqlc.arg(user_id),id,cap))) AND deleted_at IS NULL;

-- name: ListUserMachinesInWorkspaceForUser :many
SELECT m.* FROM user_machines m
WHERE m.deleted_at IS NULL AND (
  (NOT sqlc.arg(workspace_set)::boolean AND (machine_management_allowed(sqlc.arg(user_id),m.id) OR EXISTS(
    SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap
    WHERE machine_capability_allowed(sqlc.arg(user_id),m.id,cap))))
  OR (sqlc.arg(workspace_set)::boolean AND sqlc.narg(workspace_team_id)::text IS NULL
    AND m.user_id=sqlc.arg(user_id) AND m.owner_team_id IS NULL)
  OR (sqlc.arg(workspace_set)::boolean AND sqlc.narg(workspace_team_id)::text IS NOT NULL
    AND EXISTS(SELECT 1 FROM teams t JOIN team_members member USING(team_id)
      WHERE t.team_id=sqlc.narg(workspace_team_id)::text AND t.deleted_at IS NULL AND NOT t.suspended
        AND member.account_id=sqlc.arg(user_id) AND member.active)
    AND ((m.owner_team_id=sqlc.narg(workspace_team_id)::text AND machine_management_allowed(sqlc.arg(user_id),m.id))
      OR EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap
        WHERE team_machine_capability_allowed(sqlc.arg(user_id),sqlc.narg(workspace_team_id)::text,m.id,cap)))))
ORDER BY lower(m.alias), m.id
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountUserMachinesInWorkspaceForUser :one
SELECT count(*)::integer FROM user_machines m
WHERE m.deleted_at IS NULL AND (
  (NOT sqlc.arg(workspace_set)::boolean AND (machine_management_allowed(sqlc.arg(user_id),m.id) OR EXISTS(
    SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap
    WHERE machine_capability_allowed(sqlc.arg(user_id),m.id,cap))))
  OR (sqlc.arg(workspace_set)::boolean AND sqlc.narg(workspace_team_id)::text IS NULL
    AND m.user_id=sqlc.arg(user_id) AND m.owner_team_id IS NULL)
  OR (sqlc.arg(workspace_set)::boolean AND sqlc.narg(workspace_team_id)::text IS NOT NULL
    AND EXISTS(SELECT 1 FROM teams t JOIN team_members member USING(team_id)
      WHERE t.team_id=sqlc.narg(workspace_team_id)::text AND t.deleted_at IS NULL AND NOT t.suspended
        AND member.account_id=sqlc.arg(user_id) AND member.active)
    AND ((m.owner_team_id=sqlc.narg(workspace_team_id)::text AND machine_management_allowed(sqlc.arg(user_id),m.id))
      OR EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap
        WHERE team_machine_capability_allowed(sqlc.arg(user_id),sqlc.narg(workspace_team_id)::text,m.id,cap)))));

-- name: GetUserMachineForUser :one
SELECT * FROM user_machines
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND owner_team_id IS NULL AND deleted_at IS NULL;

-- name: GetUserMachineForUpdate :one
SELECT * FROM user_machines
WHERE id = sqlc.arg(id) AND machine_management_allowed(sqlc.arg(user_id),id) AND deleted_at IS NULL FOR UPDATE;

-- name: RenameUserMachine :one
UPDATE user_machines
SET alias = sqlc.arg(alias), description = sqlc.arg(description), updated_at = now(), version = version + 1
WHERE id = sqlc.arg(id) AND machine_management_allowed(sqlc.arg(user_id),id) AND deleted_at IS NULL
RETURNING *;

-- name: GetUserMachineAvailabilityOperation :one
SELECT * FROM user_machine_availability_operations
WHERE user_id = sqlc.arg(user_id) AND user_machine_id = sqlc.arg(user_machine_id)
  AND idempotency_key = sqlc.arg(idempotency_key);

-- name: CreateUserMachineAvailabilityOperation :one
INSERT INTO user_machine_availability_operations (
  user_machine_id,user_id,idempotency_key,request_hash,expected_version,resulting_version,mode,result
) VALUES (
  sqlc.arg(user_machine_id),sqlc.arg(user_id),sqlc.arg(idempotency_key),
  sqlc.arg(request_hash),sqlc.arg(expected_version),sqlc.arg(resulting_version),sqlc.arg(mode),sqlc.arg(result)
) RETURNING *;

-- name: SetUserMachineAvailabilityPolicy :execrows
UPDATE user_machines
SET availability_mode=sqlc.arg(mode),
    availability_desired_version=availability_desired_version+1,
    availability_status='pending', availability_error_code=NULL, updated_at=now()
WHERE id=sqlc.arg(id) AND machine_management_allowed(sqlc.arg(user_id),id) AND deleted_at IS NULL
  AND seat_state='occupied' AND state NOT IN ('revoked','disconnected','deleted')
  AND availability_desired_version=sqlc.arg(expected_version);

-- name: GetUserMachineAvailabilityForHelper :one
SELECT m.* FROM user_machines m
JOIN control_helpers h ON h.environment_id=m.environment_id
WHERE h.id=sqlc.arg(helper_id) AND h.environment_id=sqlc.arg(environment_id)
  AND h.state='active' AND h.revoked_at IS NULL
  AND m.deleted_at IS NULL AND m.seat_state='occupied';

-- name: RecordUserMachineAvailabilityObservation :execrows
UPDATE user_machines
SET availability_observed_mode=sqlc.arg(observed_mode),
    availability_observed_version=sqlc.arg(observed_version),
    availability_observed_at=sqlc.arg(observed_at),
    availability_status=sqlc.arg(status),
    availability_error_code=nullif(sqlc.arg(error_code),''),
    host_service_version=nullif(sqlc.arg(host_service_version),''),
    host_service_scope=nullif(sqlc.arg(host_service_scope),''),
    host_update_rollbacks=greatest(host_update_rollbacks, sqlc.arg(update_rollbacks)),
    update_health=sqlc.arg(update_health), updated_at=now()
WHERE id=sqlc.arg(id) AND environment_id=sqlc.arg(environment_id) AND deleted_at IS NULL
  AND sqlc.arg(observed_version) = availability_desired_version
  AND (sqlc.arg(status) <> 'applied' OR sqlc.arg(observed_mode) = availability_mode)
  AND (availability_observed_at IS NULL OR sqlc.arg(observed_version) > availability_observed_version OR (
    sqlc.arg(observed_version) = availability_observed_version
    AND sqlc.arg(observed_at) >= availability_observed_at
    AND sqlc.arg(update_rollbacks) >= host_update_rollbacks
  ));

-- name: GetUserMachineForBandwidthUpdate :one
SELECT * FROM user_machines
WHERE id = sqlc.arg(id) AND deleted_at IS NULL FOR UPDATE;

-- name: GetUserMachineIDForRoute :one
SELECT id FROM user_machines
WHERE provider_route_route_id = sqlc.arg(provider_route_route_id)
  AND deleted_at IS NULL;

-- name: UpdateUserMachineStatus :execrows
UPDATE user_machines
SET state = sqlc.arg(state), online = sqlc.arg(online), last_seen_at = now(),
    runtime_versions = sqlc.arg(runtime_versions), updated_at = now(), version = version + 1
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND deleted_at IS NULL
  AND state NOT IN ('revoked','deleted');

-- name: MarkUserMachineOnlineFromHelper :execrows
UPDATE user_machines
SET state = 'online', online = true, observed_capabilities = COALESCE(sqlc.narg(observed_capabilities), '{}'::text[]), last_seen_at = now(), updated_at = now(), version = version + 1
WHERE id = sqlc.arg(id) AND environment_id = sqlc.arg(environment_id)
  AND seat_state = 'occupied' AND deleted_at IS NULL AND state IN ('pending','offline','online');

-- name: GetUserMachineRuntimeInstanceForUpdate :one
SELECT online, last_seen_at, os_boot_id, worker_generation
FROM user_machines
WHERE id = sqlc.arg(id) AND environment_id = sqlc.arg(environment_id)
  AND seat_state = 'occupied' AND deleted_at IS NULL
FOR UPDATE;

-- name: MarkStaleUserMachinesOffline :execrows
UPDATE user_machines
SET state = 'offline', online = false, observed_capabilities = '{}'::text[], updated_at = now(), version = version + 1
WHERE state = 'online' AND online = true AND last_seen_at < sqlc.arg(cutoff);

-- name: MarkPairingInstallationReady :execrows
UPDATE user_machine_pairings
SET installation_state = 'ready', updated_at = now()
WHERE user_machine_id = sqlc.arg(user_machine_id)
  AND installation_state IN ('approved','material_issued','installing','connecting');

-- name: GetUserMachineCapabilityOperation :one
SELECT * FROM user_machine_capability_operations
WHERE user_id = sqlc.arg(user_id) AND user_machine_id = sqlc.arg(user_machine_id)
  AND idempotency_key = sqlc.arg(idempotency_key);

-- name: CreateUserMachineCapabilityOperation :one
INSERT INTO user_machine_capability_operations
  (user_machine_id,user_id,idempotency_key,request_hash,expected_version,resulting_version,configured_capabilities,result)
VALUES
  (sqlc.arg(user_machine_id),sqlc.arg(user_id),sqlc.arg(idempotency_key),sqlc.arg(request_hash),sqlc.arg(expected_version),sqlc.arg(resulting_version),sqlc.arg(configured_capabilities),sqlc.arg(result))
RETURNING *;

-- name: SetUserMachineCapabilities :execrows
UPDATE user_machines
SET configured_capabilities = sqlc.arg(configured_capabilities),
    capabilities_desired_version = capabilities_desired_version + 1,
    capabilities_status = CASE WHEN online THEN 'pending' ELSE 'offline' END,
    capabilities_error_code = NULL,
    updated_at = now(), version = version + 1
WHERE id = sqlc.arg(id) AND machine_management_allowed(sqlc.arg(user_id),id)
  AND capabilities_desired_version = sqlc.arg(expected_version)
  AND deleted_at IS NULL AND state NOT IN ('revoked','deleted');

-- name: RecordUserMachineCapabilitiesObservation :execrows
UPDATE user_machines
SET observed_capabilities = sqlc.arg(observed_capabilities),
    capabilities_observed_version = sqlc.arg(observed_version),
    capabilities_status = sqlc.arg(status),
    capabilities_error_code = sqlc.narg(error_code),
    updated_at = now(), version = version + 1
WHERE id = sqlc.arg(id) AND environment_id = sqlc.arg(environment_id)
  AND capabilities_desired_version = sqlc.arg(observed_version)
  AND capabilities_observed_version <= sqlc.arg(observed_version)
  AND deleted_at IS NULL AND state NOT IN ('revoked','deleted');

-- name: RecordUserMachineRuntimeDiagnostics :execrows
UPDATE user_machines
SET worker_generation = sqlc.arg(worker_generation),
    os_boot_id = sqlc.arg(os_boot_id),
    worker_service_scope = sqlc.arg(worker_service_scope),
    connector_state = sqlc.arg(connector_state),
    connector_generation = sqlc.arg(connector_generation),
    runtime_diagnostics_observed_at = sqlc.arg(observed_at),
    updated_at = now()
WHERE id = sqlc.arg(id) AND environment_id = sqlc.arg(environment_id)
  AND deleted_at IS NULL
  AND (runtime_diagnostics_observed_at IS NULL OR runtime_diagnostics_observed_at <= sqlc.arg(observed_at));

-- name: GetUserMachineRuntimeMetrics :one
SELECT
  count(*) FILTER (WHERE availability_status IN ('pending','error') OR availability_observed_version < availability_desired_version)::bigint AS availability_drift_depth,
  count(*) FILTER (WHERE availability_status = 'error')::bigint AS privileged_service_error_depth,
  count(*) FILTER (WHERE host_service_scope IS NOT NULL AND host_service_scope <> 'system')::bigint AS unsupported_host_scope_depth,
  coalesce(max(extract(epoch FROM (now() - last_seen_at))) FILTER (WHERE last_seen_at IS NOT NULL), 0)::bigint AS heartbeat_oldest_age_seconds,
  coalesce(sum(host_update_rollbacks), 0)::bigint AS update_rollbacks_total,
  (SELECT count(*)::bigint FROM user_machine_pairings WHERE installation_state = 'failed_retryable') AS bootstrap_failure_depth
FROM user_machines
WHERE deleted_at IS NULL;

-- name: SetUserMachineRoute :execrows
UPDATE user_machines
SET provider_route_route_id = sqlc.arg(provider_route_route_id), provider_route_client_id = sqlc.arg(provider_route_client_id),
    provider_route_http_base_url = sqlc.arg(provider_route_http_base_url), provider_route_websocket_base_url = sqlc.arg(provider_route_websocket_base_url),
    updated_at = now(), version = version + 1
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND deleted_at IS NULL;

-- name: SetUserMachineInstallationConfig :execrows
UPDATE user_machine_pairings
SET installation_config_ciphertext = sqlc.arg(ciphertext), installation_state='material_issued', updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'approved' AND installation_config_ciphertext IS NULL;

-- name: BindAuthenticatedMachineInstallationHelperEnrollment :execrows
UPDATE user_machine_pairings AS pairing
SET authenticated_setup_helper_enrollment_id = sqlc.arg(helper_enrollment_id), updated_at = now()
WHERE pairing.id = sqlc.arg(id)
  AND pairing.authenticated_setup_cli_session_id IS NOT NULL
  AND pairing.state IN ('approved', 'consumed')
  AND NOT EXISTS (
    SELECT 1
    FROM user_machine_pairings AS other_pairing
    WHERE other_pairing.id <> pairing.id
      AND other_pairing.authenticated_setup_helper_enrollment_id = sqlc.arg(helper_enrollment_id)
      AND other_pairing.authenticated_setup_cli_session_id IS NOT NULL
      AND other_pairing.state IN ('approved', 'consumed')
  )
  AND (
    pairing.authenticated_setup_helper_enrollment_id IS NULL
    OR pairing.authenticated_setup_helper_enrollment_id = sqlc.arg(helper_enrollment_id)
    OR (
      pairing.state = 'consumed'
      AND pairing.installation_recovery_operation_key IS NOT NULL
      AND EXISTS (
        SELECT 1
        FROM control_helper_enrollments AS previous_enrollment
        LEFT JOIN control_helpers AS previous_helper
          ON previous_helper.id = previous_enrollment.helper_id
         AND previous_helper.environment_id = previous_enrollment.environment_id
        WHERE previous_enrollment.id = pairing.authenticated_setup_helper_enrollment_id
          AND (
            previous_enrollment.state IN ('expired', 'revoked')
            OR (previous_enrollment.state = 'pending'
              AND (previous_enrollment.revoked_at IS NOT NULL OR previous_enrollment.expires_at <= now()))
            OR (previous_enrollment.state = 'consumed'
              AND (previous_helper.id IS NULL OR previous_helper.state <> 'active' OR previous_helper.revoked_at IS NOT NULL))
          )
      )
    )
  )
  AND EXISTS (
    SELECT 1
    FROM control_helper_enrollments AS enrollment
    JOIN user_machines AS machine ON machine.id = pairing.user_machine_id
    WHERE enrollment.id = sqlc.arg(helper_enrollment_id)
      AND enrollment.environment_id = machine.environment_id
      AND enrollment.state = 'pending'
      AND enrollment.revoked_at IS NULL
      AND enrollment.expires_at > now()
  );

-- name: ConsumeUserMachineInstallationConfig :one
WITH consumed AS (
  UPDATE user_machine_pairings
  SET state = 'consumed', installation_state='installing', installation_config_consumed_at = now(), updated_at = now()
  WHERE verifier_hash = sqlc.arg(verifier_hash)
    AND state = 'approved'
    AND (
      (authenticated_setup_cli_session_id IS NULL
        AND (sqlc.arg(public_identity_key) = '' OR public_identity_key = sqlc.arg(public_identity_key)))
      OR
      (authenticated_setup_cli_session_id IS NOT NULL
        AND sqlc.arg(public_identity_key) <> ''
        AND public_identity_key = sqlc.arg(public_identity_key))
    )
    AND installation_config_ciphertext IS NOT NULL
    AND installation_config_consumed_at IS NULL
    AND expires_at > now()
  RETURNING id, installation_config_ciphertext
)
SELECT installation_config_ciphertext FROM consumed;

-- name: GetUserMachineInstallationConfigForReplay :one
SELECT pairing.installation_config_ciphertext
FROM user_machine_pairings AS pairing
WHERE pairing.verifier_hash = sqlc.arg(verifier_hash)
  AND (
    (pairing.authenticated_setup_cli_session_id IS NULL
      AND (sqlc.arg(public_identity_key) = '' OR pairing.public_identity_key = sqlc.arg(public_identity_key)))
    OR
    (pairing.authenticated_setup_cli_session_id IS NOT NULL
      AND sqlc.arg(public_identity_key) <> ''
      AND pairing.public_identity_key = sqlc.arg(public_identity_key))
  )
  AND pairing.state = 'consumed'
  AND pairing.installation_config_ciphertext IS NOT NULL
  AND pairing.installation_config_consumed_at IS NOT NULL
  AND pairing.installation_recovery_operation_key IS NULL
  AND pairing.expires_at > sqlc.arg(now)
  AND pairing.installation_state IN ('material_issued','installing','connecting')
FOR UPDATE OF pairing;

-- name: BeginUserMachineInstallationRecovery :one
UPDATE user_machine_pairings
SET state = 'consumed',
    installation_recovery_operation_key = coalesce(installation_recovery_operation_key, sqlc.arg(operation_key)),
    expires_at = CASE WHEN state = 'expired' OR installation_recovery_operation_key IS NULL THEN sqlc.arg(expires_at) ELSE expires_at END,
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND verifier_hash = sqlc.arg(verifier_hash)
  AND public_identity_key = sqlc.arg(public_identity_key)
  AND state IN ('consumed', 'expired')
  AND installation_config_consumed_at IS NOT NULL
  AND installation_config_consumed_at > sqlc.arg(recovery_after)
RETURNING *;

-- name: RenewPairingInstallationRecovery :execrows
UPDATE user_machine_pairings
SET expires_at = sqlc.arg(expires_at), updated_at = now()
WHERE id = sqlc.arg(pairing_id)
  AND (installation_state IN ('material_issued','installing','connecting')
       OR (installation_state='ready' AND sqlc.arg(runtime_enrolled)::boolean));

-- name: CompleteUserMachineInstallationRecovery :execrows
UPDATE user_machine_pairings
SET installation_config_ciphertext = sqlc.arg(ciphertext),
    expires_at = sqlc.arg(expires_at),
    installation_recovery_operation_key = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND verifier_hash = sqlc.arg(verifier_hash)
  AND public_identity_key = sqlc.arg(public_identity_key)
  AND state = 'consumed'
  AND installation_config_consumed_at IS NOT NULL
  AND installation_recovery_operation_key = sqlc.arg(operation_key);

-- name: RevokeUserMachine :execrows
UPDATE user_machines
SET state = sqlc.arg(state), online = false, seat_state = sqlc.arg(seat_state),
    revoked_at = CASE WHEN sqlc.arg(state) = 'revoked' THEN now() ELSE revoked_at END,
    disconnected_at = CASE WHEN sqlc.arg(state) = 'disconnected' THEN now() ELSE disconnected_at END,
    updated_at = now(), version = version + 1
WHERE id = sqlc.arg(id) AND machine_management_allowed(sqlc.arg(user_id),id) AND deleted_at IS NULL;

-- name: DeleteUserMachine :execrows
UPDATE user_machines
SET state = 'deleted', online = false, seat_state = 'released', deleted_at = now(),
    updated_at = now(), version = version + 1
WHERE id = sqlc.arg(id) AND machine_management_allowed(sqlc.arg(user_id),id) AND deleted_at IS NULL;

-- name: UpsertUserMachineBandwidthPeriod :one
INSERT INTO user_machine_bandwidth_periods (
  user_machine_id, period_start, period_end, included_bytes
) VALUES (
  sqlc.arg(user_machine_id), sqlc.arg(period_start),
  sqlc.arg(period_end), sqlc.arg(included_bytes)
) ON CONFLICT (user_machine_id, period_start) DO UPDATE SET updated_at = now()
RETURNING *;

-- name: GetUserMachineBandwidthPeriodForUpdate :one
SELECT * FROM user_machine_bandwidth_periods
WHERE user_machine_id = sqlc.arg(user_machine_id)
  AND period_start = sqlc.arg(period_start) FOR UPDATE;

-- name: GetUserMachineForEnvironmentBandwidthUpdate :one
SELECT * FROM user_machines
WHERE environment_id = sqlc.arg(environment_id) AND deleted_at IS NULL
FOR UPDATE;

-- name: ConsumeUserMachineIncludedBandwidth :execrows
UPDATE user_machine_bandwidth_periods
SET consumed_included_bytes = consumed_included_bytes + sqlc.arg(bytes), updated_at = now()
WHERE user_machine_id = sqlc.arg(user_machine_id) AND period_start = sqlc.arg(period_start) AND consumed_included_bytes + sqlc.arg(bytes) <= included_bytes;

-- name: ListActiveUserMachineTopupsForUpdate :many
SELECT * FROM user_machine_bandwidth_topups
WHERE user_id = sqlc.arg(user_id) AND state = 'active' AND remaining_bytes > 0
  AND (expires_at IS NULL OR expires_at > now())
ORDER BY created_at, id FOR UPDATE;

-- name: ConsumeUserMachineTopup :execrows
UPDATE user_machine_bandwidth_topups
SET remaining_bytes = remaining_bytes - sqlc.arg(bytes),
    state = CASE WHEN remaining_bytes - sqlc.arg(bytes) = 0 THEN 'exhausted' ELSE 'active' END,
    consumed_at = CASE WHEN remaining_bytes - sqlc.arg(bytes) = 0 THEN now() ELSE consumed_at END,
    updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'active' AND remaining_bytes >= sqlc.arg(bytes);

-- name: RecordUserMachineTopupConsumption :execrows
UPDATE user_machine_bandwidth_periods
SET consumed_topup_bytes = consumed_topup_bytes + sqlc.arg(bytes), updated_at = now()
WHERE user_machine_id = sqlc.arg(user_machine_id) AND period_start = sqlc.arg(period_start);

-- name: CreateUserMachineBandwidthTopup :execrows
INSERT INTO user_machine_bandwidth_topups (
  id, user_id, provider_order_id, purchased_bytes, remaining_bytes, expires_at
) VALUES (
  sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(provider_order_id), sqlc.arg(purchased_bytes),
  sqlc.arg(purchased_bytes), sqlc.arg(expires_at)
) ON CONFLICT (provider_order_id) DO NOTHING;

-- name: VoidUserMachineBandwidthTopup :execrows
UPDATE user_machine_bandwidth_topups
SET state = 'void', remaining_bytes = 0, consumed_at = now(), updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND provider_order_id = sqlc.arg(provider_order_id)
  AND state = 'active';

-- name: CreateUserMachineAccessSession :exec
INSERT INTO user_machine_access_sessions (
  credential_kind,id, user_machine_id, user_id, environment_id, cli_client_session_id,
  http_base_url, helper_terminal_session_id, helper_file_session_id, expires_at, team_id, capabilities, operation_id,
  terminal_session_id,terminal_role,terminal_grant_generation,terminal_binding_generation,terminal_membership_generation,terminal_team_generation
) VALUES (
  sqlc.arg(credential_kind),sqlc.arg(id), sqlc.arg(user_machine_id), sqlc.arg(user_id), sqlc.arg(environment_id), sqlc.arg(cli_client_session_id),
  sqlc.arg(http_base_url), nullif(sqlc.arg(helper_terminal_session_id), ''), nullif(sqlc.arg(helper_file_session_id), ''), sqlc.arg(expires_at), sqlc.narg(team_id), sqlc.arg(capabilities), sqlc.arg(operation_id),
  nullif(sqlc.arg(terminal_session_id),''),nullif(sqlc.arg(terminal_role),''),sqlc.narg(terminal_grant_generation),sqlc.narg(terminal_binding_generation),sqlc.narg(terminal_membership_generation),sqlc.narg(terminal_team_generation)
);

-- name: GetUserTransferDestinationDefault :one
SELECT m.* FROM user_transfer_destination_defaults d
JOIN user_machines m ON m.id = d.machine_id
WHERE d.user_id = sqlc.arg(user_id)
  AND m.user_id = d.user_id
  AND m.deleted_at IS NULL;

-- name: SetUserTransferDestinationDefault :one
INSERT INTO user_transfer_destination_defaults (user_id, machine_id)
SELECT sqlc.arg(user_id), m.id FROM user_machines m
WHERE m.id = sqlc.arg(machine_id)
  AND m.user_id = sqlc.arg(user_id)
  AND m.deleted_at IS NULL
  AND m.state NOT IN ('revoked', 'disconnected', 'deleted')
  AND m.seat_state = 'occupied'
  AND m.online
ON CONFLICT (user_id) DO UPDATE
SET machine_id = EXCLUDED.machine_id,
    version = user_transfer_destination_defaults.version + 1,
    updated_at = now()
RETURNING *;

-- name: ClearUserTransferDestinationDefault :execrows
DELETE FROM user_transfer_destination_defaults WHERE user_id = sqlc.arg(user_id);

-- name: UserOwnsTerminalSession :one
SELECT EXISTS (
  SELECT 1 FROM user_machine_terminal_sessions s
  JOIN user_machines m ON m.id = s.user_machine_id
  WHERE s.id = sqlc.arg(session_id) AND s.owner_account = sqlc.arg(user_id) AND s.deleted_at IS NULL
) AS owned;

-- name: GetUserMachineTerminalSessionHostForUser :one
SELECT m.* FROM user_machine_terminal_sessions s
JOIN user_machines m ON m.id = s.user_machine_id
WHERE s.id = sqlc.arg(session_id) AND s.owner_account = sqlc.arg(user_id)
  AND s.deleted_at IS NULL AND m.deleted_at IS NULL;

-- name: GetTerminalSessionTransferDestination :one
SELECT m.* FROM user_machine_terminal_sessions s
JOIN user_machines owner ON owner.id = s.user_machine_id
JOIN user_machines m ON m.id = s.transfer_destination_machine_id
WHERE s.id = sqlc.arg(session_id) AND s.owner_account = sqlc.arg(user_id) AND s.deleted_at IS NULL
LIMIT 1;

-- name: SetTerminalSessionTransferDestination :one
WITH destination AS (
  SELECT candidate.id FROM user_machines candidate
  WHERE candidate.id = sqlc.arg(machine_id) AND candidate.user_id = sqlc.arg(user_id)
    AND candidate.deleted_at IS NULL AND candidate.state NOT IN ('revoked','disconnected','deleted')
    AND candidate.seat_state = 'occupied'
), machine_updated AS (
  UPDATE user_machine_terminal_sessions s SET transfer_destination_machine_id = destination.id, version = s.version + 1, updated_at = now()
  FROM user_machines owner, destination
  WHERE s.id = sqlc.arg(session_id) AND s.user_machine_id = owner.id AND s.owner_account = sqlc.arg(user_id) AND s.deleted_at IS NULL
  RETURNING destination.id
)
SELECT m.* FROM user_machines m
WHERE m.id = (SELECT id FROM machine_updated LIMIT 1);

-- name: ClearTerminalSessionTransferDestination :one
WITH machine_updated AS (
  UPDATE user_machine_terminal_sessions s SET transfer_destination_machine_id = NULL, version = s.version + 1, updated_at = now()
  FROM user_machines owner
  WHERE s.id = sqlc.arg(session_id) AND s.user_machine_id = owner.id AND s.owner_account = sqlc.arg(user_id) AND s.deleted_at IS NULL
  RETURNING s.id
)
SELECT id FROM machine_updated LIMIT 1;

-- name: RevokeUserMachineAccessSessions :many
UPDATE user_machine_access_sessions
SET state = 'revoked', revoked_at = coalesce(revoked_at, now()), revocation_reason = sqlc.arg(reason), updated_at = now()
WHERE user_machine_id = sqlc.arg(user_machine_id)
  AND (state = 'active' OR (state = 'revoked' AND helper_revoked_at IS NULL))
RETURNING id, user_id, user_machine_id, environment_id, cli_client_session_id,
  http_base_url, coalesce(helper_terminal_session_id, '') AS helper_terminal_session_id,
  coalesce(helper_file_session_id, '') AS helper_file_session_id, revocation_reason;

-- name: RevokeUserMachineAccessSessionsForUser :many
UPDATE user_machine_access_sessions
SET state = 'revoked', revoked_at = coalesce(revoked_at, now()), revocation_reason = sqlc.arg(reason), updated_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND (state = 'active' OR (state = 'revoked' AND helper_revoked_at IS NULL))
RETURNING id, user_id, user_machine_id, environment_id, cli_client_session_id,
  http_base_url, coalesce(helper_terminal_session_id, '') AS helper_terminal_session_id,
  coalesce(helper_file_session_id, '') AS helper_file_session_id, revocation_reason;

-- name: ListPendingUserMachineAccessSessionRevocations :many
SELECT id, user_id, user_machine_id, environment_id, cli_client_session_id,
  http_base_url, coalesce(helper_terminal_session_id, '') AS helper_terminal_session_id,
  coalesce(helper_file_session_id, '') AS helper_file_session_id, revocation_reason
FROM user_machine_access_sessions
WHERE state = 'revoked' AND helper_revoked_at IS NULL
ORDER BY created_at, id;

-- name: MarkUserMachineAccessSessionRevoked :execrows
UPDATE user_machine_access_sessions
SET helper_revoked_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'revoked' AND helper_revoked_at IS NULL;

-- name: GetUserMachineTerminalSession :one
SELECT s.* FROM user_machine_terminal_sessions s
JOIN user_machines m ON m.id=s.user_machine_id
WHERE s.id=sqlc.arg(id) AND s.user_machine_id=sqlc.arg(user_machine_id)
  AND s.owner_account=sqlc.arg(user_id) AND machine_capability_allowed(sqlc.arg(user_id),m.id,'terminal') AND m.deleted_at IS NULL AND s.deleted_at IS NULL;

-- name: GetUserMachineTerminalSessionInWorkspace :one
SELECT s.* FROM user_machine_terminal_sessions s
JOIN user_machines m ON m.id=s.user_machine_id
WHERE s.id=sqlc.arg(id) AND s.user_machine_id=sqlc.arg(user_machine_id)
  AND s.owner_account=sqlc.arg(user_id) AND s.owner_team_id IS NOT DISTINCT FROM sqlc.narg(workspace_team_id)::text
  AND m.deleted_at IS NULL AND s.deleted_at IS NULL
  AND ((sqlc.narg(workspace_team_id)::text IS NULL AND m.user_id=sqlc.arg(user_id) AND m.owner_team_id IS NULL
        AND machine_capability_allowed(sqlc.arg(user_id),m.id,'terminal'))
    OR (sqlc.narg(workspace_team_id)::text IS NOT NULL
        AND team_machine_capability_allowed(sqlc.arg(user_id),sqlc.narg(workspace_team_id)::text,m.id,'terminal')));

-- name: GetUserMachineForSharedTerminalSession :one
WITH target AS (
 SELECT s.user_machine_id machine_id FROM user_machine_terminal_sessions s WHERE s.id=sqlc.arg(session_id) AND s.deleted_at IS NULL
)
SELECT m.* FROM target JOIN user_machines m ON m.id=target.machine_id
WHERE terminal_session_role(sqlc.arg(user_id),sqlc.arg(session_id)) IN ('viewer','interactive') AND m.deleted_at IS NULL;

-- name: GetSharedUserMachineTerminalSession :one
SELECT s.* FROM user_machine_terminal_sessions s WHERE s.id=sqlc.arg(session_id) AND s.deleted_at IS NULL AND terminal_session_role(sqlc.arg(user_id),s.id) IN ('viewer','interactive');

-- name: ListUserMachineTerminalSessions :many
SELECT s.* FROM user_machine_terminal_sessions s
JOIN user_machines m ON m.id=s.user_machine_id
WHERE s.user_machine_id=sqlc.arg(user_machine_id) AND s.owner_account=sqlc.arg(user_id) AND machine_capability_allowed(sqlc.arg(user_id),m.id,'terminal')
  AND m.deleted_at IS NULL AND s.deleted_at IS NULL
ORDER BY s.is_default DESC, s.last_activity_at DESC NULLS LAST, s.name;

-- name: ListUserMachineTerminalSessionsInWorkspace :many
SELECT s.* FROM user_machine_terminal_sessions s
JOIN user_machines m ON m.id=s.user_machine_id
WHERE s.user_machine_id=sqlc.arg(user_machine_id) AND s.owner_account=sqlc.arg(user_id)
  AND s.owner_team_id IS NOT DISTINCT FROM sqlc.narg(workspace_team_id)::text
  AND m.deleted_at IS NULL AND s.deleted_at IS NULL
  AND ((sqlc.narg(workspace_team_id)::text IS NULL AND m.user_id=sqlc.arg(user_id) AND m.owner_team_id IS NULL
        AND machine_capability_allowed(sqlc.arg(user_id),m.id,'terminal'))
    OR (sqlc.narg(workspace_team_id)::text IS NOT NULL
        AND team_machine_capability_allowed(sqlc.arg(user_id),sqlc.narg(workspace_team_id)::text,m.id,'terminal')))
ORDER BY s.is_default DESC, s.last_activity_at DESC NULLS LAST, s.name;

-- name: GetUserMachineTerminalSessionByIdempotencyKey :one
SELECT s.* FROM user_machine_terminal_sessions s
JOIN user_machines m ON m.id=s.user_machine_id
WHERE s.user_machine_id=sqlc.arg(user_machine_id) AND s.owner_account=sqlc.arg(user_id) AND machine_capability_allowed(sqlc.arg(user_id),m.id,'terminal')
  AND s.idempotency_key=sqlc.arg(idempotency_key) AND s.deleted_at IS NULL;

-- name: GetUserMachineTerminalSessionByIdempotencyKeyInWorkspace :one
SELECT s.* FROM user_machine_terminal_sessions s
JOIN user_machines m ON m.id=s.user_machine_id
WHERE s.user_machine_id=sqlc.arg(user_machine_id) AND s.owner_account=sqlc.arg(user_id)
  AND s.owner_team_id IS NOT DISTINCT FROM sqlc.narg(workspace_team_id)::text
  AND s.idempotency_key=sqlc.arg(idempotency_key) AND s.deleted_at IS NULL
  AND ((sqlc.narg(workspace_team_id)::text IS NULL AND m.user_id=sqlc.arg(user_id) AND m.owner_team_id IS NULL
        AND machine_capability_allowed(sqlc.arg(user_id),m.id,'terminal'))
    OR (sqlc.narg(workspace_team_id)::text IS NOT NULL
        AND team_machine_capability_allowed(sqlc.arg(user_id),sqlc.narg(workspace_team_id)::text,m.id,'terminal')));

-- name: LockUserMachineTerminalSessions :one
SELECT id FROM user_machines
WHERE id=sqlc.arg(user_machine_id) AND machine_capability_allowed(sqlc.arg(user_id),id,'terminal') AND deleted_at IS NULL FOR UPDATE;

-- name: LockUserMachineTerminalSessionsInWorkspace :one
SELECT id FROM user_machines m
WHERE m.id=sqlc.arg(user_machine_id) AND m.deleted_at IS NULL
  AND ((sqlc.narg(workspace_team_id)::text IS NULL AND m.user_id=sqlc.arg(user_id) AND m.owner_team_id IS NULL
        AND machine_capability_allowed(sqlc.arg(user_id),m.id,'terminal'))
    OR (sqlc.narg(workspace_team_id)::text IS NOT NULL
        AND team_machine_capability_allowed(sqlc.arg(user_id),sqlc.narg(workspace_team_id)::text,m.id,'terminal')))
FOR UPDATE;

-- name: CountLiveUserMachineTerminalSessions :one
SELECT count(*)::integer FROM user_machine_terminal_sessions
WHERE user_machine_id=sqlc.arg(user_machine_id) AND deleted_at IS NULL
  AND (
    (desired_state='open' AND runtime_state NOT IN ('closed','exited','deleted'))
    OR runtime_state IN ('creating','running','restarting','closing')
  );

-- name: CreateUserMachineTerminalSession :execrows
INSERT INTO user_machine_terminal_sessions (id,user_machine_id,owner_account,owner_team_id,name,idempotency_key,launch_cwd)
VALUES (sqlc.arg(id),sqlc.arg(user_machine_id),sqlc.arg(owner_account),sqlc.narg(owner_team_id),sqlc.arg(name),sqlc.arg(idempotency_key),sqlc.arg(launch_cwd))
ON CONFLICT DO NOTHING;

-- name: RenameUserMachineTerminalSession :execrows
UPDATE user_machine_terminal_sessions SET name=sqlc.arg(name),version=version+1,updated_at=now()
WHERE user_machine_id=sqlc.arg(user_machine_id) AND id=sqlc.arg(id)
  AND owner_account=sqlc.arg(owner_account) AND NOT is_default AND deleted_at IS NULL;

-- name: RenameUserMachineTerminalSessionInWorkspace :execrows
UPDATE user_machine_terminal_sessions SET name=sqlc.arg(name),version=version+1,updated_at=now()
WHERE user_machine_id=sqlc.arg(user_machine_id) AND id=sqlc.arg(id)
  AND owner_account=sqlc.arg(owner_account) AND owner_team_id IS NOT DISTINCT FROM sqlc.narg(workspace_team_id)::text
  AND NOT is_default AND deleted_at IS NULL;

-- name: CloseUserMachineTerminalSession :execrows
UPDATE user_machine_terminal_sessions SET desired_state='closed',version=version+1,updated_at=now()
WHERE user_machine_id=sqlc.arg(user_machine_id) AND id=sqlc.arg(id)
  AND deleted_at IS NULL AND desired_state<>'closed';

-- name: CloseUserMachineTerminalSessionInWorkspace :execrows
UPDATE user_machine_terminal_sessions SET desired_state='closed',version=version+1,updated_at=now()
WHERE user_machine_id=sqlc.arg(user_machine_id) AND id=sqlc.arg(id)
  AND owner_team_id IS NOT DISTINCT FROM sqlc.narg(workspace_team_id)::text
  AND deleted_at IS NULL AND desired_state<>'closed';

-- name: DeleteUserMachineTerminalSession :execrows
UPDATE user_machine_terminal_sessions
SET desired_state='deleted',deleted_at=now(),title=NULL,launch_cwd='',last_activity_at=NULL,
    last_runtime_sequence=NULL,transfer_destination_machine_id=NULL,version=version+1,updated_at=now()
WHERE user_machine_id=sqlc.arg(user_machine_id) AND id=sqlc.arg(id)
  AND deleted_at IS NULL AND NOT is_default;

-- name: DeleteUserMachineTerminalSessionInWorkspace :execrows
UPDATE user_machine_terminal_sessions
SET desired_state='deleted',deleted_at=now(),title=NULL,launch_cwd='',last_activity_at=NULL,
    last_runtime_sequence=NULL,transfer_destination_machine_id=NULL,version=version+1,updated_at=now()
WHERE user_machine_id=sqlc.arg(user_machine_id) AND id=sqlc.arg(id)
  AND owner_team_id IS NOT DISTINCT FROM sqlc.narg(workspace_team_id)::text
  AND deleted_at IS NULL AND NOT is_default;

-- name: ExpireClosedUserMachineTerminalSessions :many
WITH expired AS (
  SELECT session.id
  FROM user_machine_terminal_sessions AS session
  WHERE session.deleted_at IS NULL AND session.closed_at IS NOT NULL
    AND session.closed_at <= sqlc.arg(expired_before)
    AND session.runtime_state IN ('closed','exited','deleted')
    AND NOT session.is_default
  ORDER BY session.closed_at,session.id
  LIMIT sqlc.arg(batch_size)
  FOR UPDATE SKIP LOCKED
), updated AS (
UPDATE user_machine_terminal_sessions AS session
SET desired_state='deleted',deleted_at=now(),title=NULL,launch_cwd='',last_activity_at=NULL,
    last_runtime_sequence=NULL,transfer_destination_machine_id=NULL,version=session.version+1,updated_at=now()
FROM expired
WHERE session.id=expired.id AND session.deleted_at IS NULL
  AND session.runtime_state IN ('closed','exited','deleted') AND NOT session.is_default
RETURNING session.id,session.user_machine_id
)
SELECT id,user_machine_id FROM updated;

-- name: QueueUserMachineTerminalSessionOperation :exec
INSERT INTO user_machine_terminal_session_operations (id,user_machine_id,terminal_session_id,operation)
VALUES (sqlc.arg(id),sqlc.arg(user_machine_id),sqlc.arg(terminal_session_id),sqlc.arg(operation))
ON CONFLICT (terminal_session_id,operation) WHERE state='pending' DO NOTHING;

-- name: UserMachineTerminalSessionOperationPending :one
SELECT EXISTS (
  SELECT 1 FROM user_machine_terminal_session_operations
  WHERE user_machine_id=sqlc.arg(user_machine_id)
    AND terminal_session_id=sqlc.arg(terminal_session_id) AND state='pending'
);

-- name: ListDueUserMachineTerminalSessionOperations :many
SELECT o.id,o.user_machine_id,o.terminal_session_id,o.operation,o.attempts,
  s.owner_account AS user_id,m.environment_id,coalesce((SELECT 'https://' || r.public_host
    FROM control_routes r
    JOIN control_environments e ON e.id = r.environment_id
    JOIN control_connector_generations c ON c.environment_id = r.environment_id AND c.connector_id = r.connector_id
    JOIN user_machines connector_machine ON connector_machine.id = c.machine_id AND connector_machine.environment_id = r.environment_id
    JOIN control_tunnel_nodes n ON n.id = c.edge_node_id AND n.id = r.applied_node_id
    WHERE r.environment_id = m.environment_id AND r.kind = 'runtime_https_wss'
      AND r.desired_state IN ('attached','replacing') AND r.applied_revision >= r.desired_revision
      AND r.applied_generation = c.generation AND e.desired_state = 'active'
      AND connector_machine.revoked_at IS NULL AND connector_machine.deleted_at IS NULL
      AND c.state = 'admitted' AND n.state = 'ready' AND n.ready = true
    ORDER BY r.id LIMIT 1), '')::text AS provider_route_http_base_url
FROM user_machine_terminal_session_operations o
JOIN user_machines m ON m.id=o.user_machine_id
JOIN user_machine_terminal_sessions s ON s.id=o.terminal_session_id
WHERE o.state='pending' AND o.next_attempt_at<=now()
ORDER BY o.created_at LIMIT sqlc.arg(batch_size);

-- name: ListPendingUserMachineTerminalSessionOperations :many
SELECT o.id,o.user_machine_id,o.terminal_session_id,o.operation,o.attempts,
  s.owner_account AS user_id,m.environment_id,coalesce((SELECT 'https://' || r.public_host
    FROM control_routes r
    JOIN control_environments e ON e.id = r.environment_id
    JOIN control_connector_generations c ON c.environment_id = r.environment_id AND c.connector_id = r.connector_id
    JOIN user_machines connector_machine ON connector_machine.id = c.machine_id AND connector_machine.environment_id = r.environment_id
    JOIN control_tunnel_nodes n ON n.id = c.edge_node_id AND n.id = r.applied_node_id
    WHERE r.environment_id = m.environment_id AND r.kind = 'runtime_https_wss'
      AND r.desired_state IN ('attached','replacing') AND r.applied_revision >= r.desired_revision
      AND r.applied_generation = c.generation AND e.desired_state = 'active'
      AND connector_machine.revoked_at IS NULL AND connector_machine.deleted_at IS NULL
      AND c.state = 'admitted' AND n.state = 'ready' AND n.ready = true
    ORDER BY r.id LIMIT 1), '')::text AS provider_route_http_base_url
FROM user_machine_terminal_session_operations o
JOIN user_machines m ON m.id=o.user_machine_id
JOIN user_machine_terminal_sessions s ON s.id=o.terminal_session_id
WHERE o.user_machine_id=sqlc.arg(user_machine_id) AND o.state='pending'
ORDER BY o.created_at LIMIT sqlc.arg(batch_size);

-- name: MarkUserMachineTerminalSessionOperationApplied :exec
UPDATE user_machine_terminal_session_operations
SET state='applied',completed_at=now(),updated_at=now(),last_error=NULL
WHERE id=sqlc.arg(id) AND state='pending';

-- name: MarkUserMachineTerminalSessionRuntimeClosed :exec
UPDATE user_machine_terminal_sessions
SET runtime_state='closed',closed_at=coalesce(closed_at,now()),updated_at=now(),version=version+1
WHERE id=sqlc.arg(id) AND deleted_at IS NULL;

-- name: RetryUserMachineTerminalSessionOperation :exec
UPDATE user_machine_terminal_session_operations
SET attempts=attempts+1,next_attempt_at=now()+make_interval(secs => sqlc.arg(retry_seconds)),last_error=sqlc.arg(last_error),updated_at=now()
WHERE id=sqlc.arg(id) AND state='pending';
-- name: GetUserMachineForCapability :one
SELECT m.* FROM user_machines m
WHERE m.id=sqlc.arg(id) AND m.deleted_at IS NULL
AND machine_capability_allowed(sqlc.arg(user_id),m.id,sqlc.arg(capability));

-- name: GetVisibleUserMachine :one
SELECT m.* FROM user_machines m WHERE m.id=sqlc.arg(id) AND m.deleted_at IS NULL
AND (machine_management_allowed(sqlc.arg(user_id),m.id) OR EXISTS(
 SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) capability
 WHERE machine_capability_allowed(sqlc.arg(user_id),m.id,capability)));

-- name: ListUserMachinesByIDs :many
-- The inventory has already decided which machines the caller may see; this loads exactly those rows in one query.
SELECT m.* FROM user_machines m WHERE m.id=ANY(sqlc.arg(ids)::text[]) AND m.deleted_at IS NULL;

-- name: GetVisibleUserMachineInWorkspace :one
SELECT m.* FROM user_machines m WHERE m.id=sqlc.arg(id) AND m.deleted_at IS NULL AND (
  (NOT sqlc.arg(workspace_set)::boolean AND (machine_management_allowed(sqlc.arg(user_id),m.id) OR EXISTS(
    SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) capability
    WHERE machine_capability_allowed(sqlc.arg(user_id),m.id,capability))))
  OR (sqlc.arg(workspace_set)::boolean AND sqlc.narg(workspace_team_id)::text IS NULL
    AND m.user_id=sqlc.arg(user_id) AND m.owner_team_id IS NULL)
  OR (sqlc.arg(workspace_set)::boolean AND sqlc.narg(workspace_team_id)::text IS NOT NULL
    AND EXISTS(SELECT 1 FROM teams t JOIN team_members member USING(team_id)
      WHERE t.team_id=sqlc.narg(workspace_team_id)::text AND t.deleted_at IS NULL AND NOT t.suspended
        AND member.account_id=sqlc.arg(user_id) AND member.active)
    AND ((m.owner_team_id=sqlc.narg(workspace_team_id)::text AND machine_management_allowed(sqlc.arg(user_id),m.id))
      OR EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) capability
        WHERE team_machine_capability_allowed(sqlc.arg(user_id),sqlc.narg(workspace_team_id)::text,m.id,capability)))));

-- name: GetUserMachineForCapabilityInWorkspace :one
SELECT m.* FROM user_machines m
WHERE m.id=sqlc.arg(id) AND m.deleted_at IS NULL AND (
  (NOT sqlc.arg(workspace_set)::boolean AND machine_capability_allowed(sqlc.arg(user_id),m.id,sqlc.arg(capability)))
  OR (sqlc.arg(workspace_set)::boolean AND sqlc.narg(workspace_team_id)::text IS NULL
    AND m.user_id=sqlc.arg(user_id) AND m.owner_team_id IS NULL
    AND machine_capability_allowed(sqlc.arg(user_id),m.id,sqlc.arg(capability)))
  OR (sqlc.arg(workspace_set)::boolean AND sqlc.narg(workspace_team_id)::text IS NOT NULL
    AND team_machine_capability_allowed(sqlc.arg(user_id),sqlc.narg(workspace_team_id)::text,m.id,sqlc.arg(capability))));
