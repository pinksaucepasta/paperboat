-- name: GetManagedSSHOperationForUpdate :one
SELECT * FROM managed_ssh_operations
WHERE operation_id = sqlc.arg(operation_id)
FOR UPDATE;

-- name: CreateManagedSSHOperation :one
INSERT INTO managed_ssh_operations
  (operation_id, user_id, operation_kind, request_hash, resource_id,
   result_revision, created_at)
VALUES
  (sqlc.arg(operation_id), sqlc.arg(user_id), sqlc.arg(operation_kind),
   sqlc.arg(request_hash), sqlc.arg(resource_id), sqlc.arg(result_revision),
   sqlc.arg(created_at))
RETURNING *;

-- name: ResolveMachineSSHHostKeyAuthorityForUpdate :one
SELECT machine.id AS user_machine_id, machine.user_id, machine.installation_generation
FROM user_machines machine
JOIN users owner ON owner.id = machine.user_id
WHERE machine.id = sqlc.arg(user_machine_id)
  AND machine.user_id = sqlc.arg(user_id)
  AND machine.installation_generation = sqlc.arg(machine_generation)
  AND machine.state NOT IN ('revoked', 'deleted')
  AND machine.deleted_at IS NULL
  AND owner.status = 'active'
FOR UPDATE OF machine, owner;

-- name: GetMachineSSHTargetForUpdate :one
SELECT * FROM machine_ssh_targets
WHERE user_machine_id = sqlc.arg(user_machine_id)
FOR UPDATE;

-- name: CreateMachineSSHTarget :one
INSERT INTO machine_ssh_targets
  (user_machine_id, machine_generation, os_user, target_port,
   reconciliation_version, created_at, updated_at)
VALUES
  (sqlc.arg(user_machine_id), sqlc.arg(machine_generation), sqlc.arg(os_user),
   sqlc.arg(target_port), 1, sqlc.arg(now), sqlc.arg(now))
RETURNING *;

-- name: ReplaceMachineSSHTargetGeneration :one
UPDATE machine_ssh_targets
SET machine_generation = sqlc.arg(machine_generation), os_user = sqlc.arg(os_user),
    target_port = sqlc.arg(target_port),
    reconciliation_version = reconciliation_version + 1, updated_at = sqlc.arg(now)
WHERE user_machine_id = sqlc.arg(user_machine_id)
  AND machine_generation < sqlc.arg(machine_generation)
RETURNING *;

-- name: UpdateMachineSSHTargetPort :one
UPDATE machine_ssh_targets
SET target_port = sqlc.arg(target_port),
    reconciliation_version = reconciliation_version + 1, updated_at = sqlc.arg(now)
WHERE user_machine_id = sqlc.arg(user_machine_id)
  AND machine_generation = sqlc.arg(machine_generation)
  AND reconciliation_version = sqlc.arg(expected_reconciliation_version)
RETURNING *;

-- Several machines of one account co-own a key when they run on one computer: the observing machine's own row first.
-- name: GetMachineSSHHostKeyOwner :one
SELECT * FROM machine_ssh_host_key_owners
WHERE account_id = sqlc.arg(account_id) AND fingerprint = sqlc.arg(fingerprint)
ORDER BY (user_machine_id = sqlc.arg(user_machine_id)) DESC, first_observed_at
LIMIT 1;

-- Two machine records on one running computer (two OS users) report the same os_boot_id; a clone booted elsewhere does not.
-- name: MachinesShareRunningHost :one
SELECT EXISTS(
  SELECT 1 FROM user_machines a JOIN user_machines b ON b.id = sqlc.arg(other_machine_id)
  WHERE a.id = sqlc.arg(user_machine_id) AND a.os_boot_id IS NOT NULL AND a.os_boot_id = b.os_boot_id
    AND a.deleted_at IS NULL AND b.deleted_at IS NULL
)::boolean AS same_host;

-- name: DeleteRetiredMachineSSHHostKeys :exec
DELETE FROM machine_ssh_host_keys k
USING user_machines m
WHERE k.fingerprint = sqlc.arg(fingerprint) AND m.id = k.user_machine_id AND m.user_id = sqlc.arg(user_id)
  AND m.state IN ('revoked', 'deleted', 'disconnected');

-- name: DeleteRetiredMachineSSHHostKeyOwner :execrows
DELETE FROM machine_ssh_host_key_owners o
USING user_machines m
WHERE o.fingerprint = sqlc.arg(fingerprint) AND m.id = o.user_machine_id AND o.account_id = sqlc.arg(user_id)
  AND m.state IN ('revoked', 'deleted', 'disconnected');

-- name: CreateMachineSSHHostKeyOwner :one
INSERT INTO machine_ssh_host_key_owners
  (fingerprint, user_machine_id, account_id, algorithm, public_key, first_observed_at)
VALUES
  (sqlc.arg(fingerprint), sqlc.arg(user_machine_id), sqlc.arg(account_id), sqlc.arg(algorithm),
   sqlc.arg(public_key), sqlc.arg(first_observed_at))
ON CONFLICT DO NOTHING
RETURNING *;

-- The machine holding a fingerprint after an insert found it taken: the account's own owner first.
-- name: FindMachineSSHHostKeyOwnerMachine :one
SELECT user_machine_id FROM machine_ssh_host_key_owners
WHERE fingerprint = sqlc.arg(fingerprint)
ORDER BY (account_id = sqlc.arg(account_id)) DESC
LIMIT 1;

-- name: GetMachineSSHHostKeySetByObservation :one
SELECT * FROM machine_ssh_host_key_sets
WHERE user_machine_id = sqlc.arg(user_machine_id)
  AND machine_generation = sqlc.arg(machine_generation)
  AND observation_generation = sqlc.arg(observation_generation)
  AND state IN ('active', 'pending')
FOR UPDATE;

-- name: GetMachineSSHHostKeySetByID :one
SELECT * FROM machine_ssh_host_key_sets
WHERE id = sqlc.arg(id)
FOR UPDATE;

-- name: GetActiveMachineSSHHostKeySetForUpdate :one
SELECT * FROM machine_ssh_host_key_sets
WHERE user_machine_id = sqlc.arg(user_machine_id) AND state = 'active'
FOR UPDATE;

-- name: GetPendingMachineSSHHostKeySetForUpdate :one
SELECT * FROM machine_ssh_host_key_sets
WHERE user_machine_id = sqlc.arg(user_machine_id) AND state = 'pending'
FOR UPDATE;

-- name: CreateMachineSSHHostKeySet :one
INSERT INTO machine_ssh_host_key_sets
  (id, user_machine_id, machine_generation, observation_generation,
   set_fingerprint, state, reconciliation_version, observed_at, promoted_at)
VALUES
  (sqlc.arg(id), sqlc.arg(user_machine_id), sqlc.arg(machine_generation),
   sqlc.arg(observation_generation), sqlc.arg(set_fingerprint), sqlc.arg(state),
   sqlc.arg(reconciliation_version), sqlc.arg(observed_at), sqlc.narg(promoted_at))
RETURNING *;

-- name: AddMachineSSHHostKeyToSet :exec
INSERT INTO machine_ssh_host_keys (set_id, user_machine_id, fingerprint, ordinal)
VALUES (sqlc.arg(set_id), sqlc.arg(user_machine_id), sqlc.arg(fingerprint), sqlc.arg(ordinal));

-- name: ListMachineSSHHostKeysForSet :many
SELECT member.ordinal, owner.fingerprint, owner.algorithm, owner.public_key
FROM machine_ssh_host_keys member
JOIN machine_ssh_host_key_owners owner
  ON owner.fingerprint = member.fingerprint
 AND owner.user_machine_id = member.user_machine_id
WHERE member.set_id = sqlc.arg(set_id)
ORDER BY member.ordinal;

-- name: RejectPendingMachineSSHHostKeySet :one
UPDATE machine_ssh_host_key_sets
SET state = 'rejected', rejected_at = sqlc.arg(rejected_at),
    rejection_reason = sqlc.arg(rejection_reason),
    reconciliation_version = reconciliation_version + 1
WHERE id = sqlc.arg(id) AND state = 'pending'
RETURNING *;

-- name: SupersedeActiveMachineSSHHostKeySet :one
UPDATE machine_ssh_host_key_sets
SET state = 'superseded', reconciliation_version = reconciliation_version + 1
WHERE id = sqlc.arg(id) AND state = 'active'
RETURNING *;

-- name: PromotePendingMachineSSHHostKeySet :one
UPDATE machine_ssh_host_key_sets
SET state = 'active', promoted_at = sqlc.arg(promoted_at),
    reconciliation_version = reconciliation_version + 1
WHERE id = sqlc.arg(id) AND state = 'pending'
RETURNING *;

-- name: ResolveMachineSSHUseAuthorityForUpdate :one
SELECT m.id FROM user_machines m WHERE m.id=sqlc.arg(machine_id)
AND m.installation_generation=sqlc.arg(machine_generation)
AND (machine_capability_allowed(sqlc.arg(user_id),m.id,'managed_ssh') OR machine_management_allowed(sqlc.arg(user_id),m.id))
AND m.state NOT IN ('revoked','disconnected','deleted') FOR UPDATE;

-- name: ResolveMachineSSHManagementAuthorityForUpdate :one
SELECT m.id FROM user_machines m WHERE m.id=sqlc.arg(machine_id)
AND m.installation_generation=sqlc.arg(machine_generation)
AND machine_management_allowed(sqlc.arg(user_id),m.id)
AND m.state NOT IN ('revoked','disconnected','deleted') FOR UPDATE;

-- Reads for GET ssh-target / ssh-host-keys: one read-only snapshot, no row locks. Every daemon polls these for
-- every reachable machine, so the locking variants above serialized ~50 transactions/s on user_machines.
-- The account's own personal machine is answered by machine_management_allowed's first branch without calling it.
-- name: ResolveMachineSSHUseAuthority :one
SELECT m.id FROM user_machines m WHERE m.id=sqlc.arg(machine_id)
AND m.installation_generation=sqlc.arg(machine_generation)
AND ((m.owner_team_id IS NULL AND m.user_id=sqlc.arg(user_id) AND m.deleted_at IS NULL)
  OR machine_capability_allowed(sqlc.arg(user_id),m.id,'managed_ssh') OR machine_management_allowed(sqlc.arg(user_id),m.id))
AND m.state NOT IN ('revoked','disconnected','deleted');

-- name: GetMachineSSHTarget :one
SELECT * FROM machine_ssh_targets WHERE user_machine_id = sqlc.arg(user_machine_id);

-- name: GetActiveMachineSSHHostKeySet :one
SELECT * FROM machine_ssh_host_key_sets WHERE user_machine_id = sqlc.arg(user_machine_id) AND state = 'active';

-- name: GetPendingMachineSSHHostKeySet :one
SELECT * FROM machine_ssh_host_key_sets WHERE user_machine_id = sqlc.arg(user_machine_id) AND state = 'pending';

-- name: ResolveMachineSSHCertificateAuthority :one
-- One read for certificate issuance: the CLI session is live, the user may use managed SSH on the exact machine and
-- generation, and the machine has a target. account_id names the CA (the machine's immutable owner account).
SELECT m.user_id AS account_id, t.os_user FROM user_machines m
JOIN machine_ssh_targets t ON t.user_machine_id = m.id AND t.machine_generation = m.installation_generation
JOIN cli_client_sessions session ON session.id = sqlc.arg(cli_client_session_id) AND session.user_id = sqlc.arg(user_id)
  AND session.state = 'active' AND session.revoked_at IS NULL
WHERE m.id = sqlc.arg(machine_id) AND m.installation_generation = sqlc.arg(machine_generation)
AND ((m.owner_team_id IS NULL AND m.user_id = sqlc.arg(user_id) AND m.deleted_at IS NULL)
  OR machine_capability_allowed(sqlc.arg(user_id), m.id, 'managed_ssh') OR machine_management_allowed(sqlc.arg(user_id), m.id))
AND m.state NOT IN ('revoked', 'disconnected', 'deleted');

-- name: CreateFirstManagedSSHUserCA :exec
-- The account's first CA, made on first use; a concurrent first use keeps the row that won.
INSERT INTO managed_ssh_user_cas (id, account_id, state, public_key, private_key_ciphertext, created_at, state_changed_at)
SELECT sqlc.arg(id), sqlc.arg(account_id), 'active', sqlc.arg(public_key), sqlc.arg(private_key_ciphertext), sqlc.arg(now), sqlc.arg(now)
WHERE NOT EXISTS (SELECT 1 FROM managed_ssh_user_cas WHERE account_id = sqlc.arg(account_id))
ON CONFLICT DO NOTHING;

-- name: CreatePendingManagedSSHUserCA :exec
INSERT INTO managed_ssh_user_cas (id, account_id, state, public_key, private_key_ciphertext, created_at, state_changed_at)
VALUES (sqlc.arg(id), sqlc.arg(account_id), 'pending', sqlc.arg(public_key), sqlc.arg(private_key_ciphertext), sqlc.arg(now), sqlc.arg(now));

-- name: GetActiveManagedSSHUserCA :one
SELECT * FROM managed_ssh_user_cas WHERE account_id = sqlc.arg(account_id) AND state = 'active';

-- name: ListManagedSSHUserCAs :many
SELECT * FROM managed_ssh_user_cas WHERE account_id = sqlc.arg(account_id)
ORDER BY created_at, CASE state WHEN 'retiring' THEN 0 WHEN 'active' THEN 1 ELSE 2 END;

-- name: SetManagedSSHUserCAState :exec
UPDATE managed_ssh_user_cas SET state = sqlc.arg(new_state), state_changed_at = sqlc.arg(now)
WHERE account_id = sqlc.arg(account_id) AND state = sqlc.arg(old_state);

-- name: DeleteManagedSSHUserCA :exec
DELETE FROM managed_ssh_user_cas WHERE account_id = sqlc.arg(account_id) AND state = sqlc.arg(state);

-- name: RecordManagedSSHCertificateHolder :exec
INSERT INTO managed_ssh_certificate_holders (machine_id, cli_client_session_id, user_id, valid_before)
VALUES (sqlc.arg(machine_id), sqlc.arg(cli_client_session_id), sqlc.arg(user_id), sqlc.arg(valid_before))
ON CONFLICT (machine_id, cli_client_session_id) DO UPDATE SET valid_before = GREATEST(managed_ssh_certificate_holders.valid_before, EXCLUDED.valid_before);

-- name: ListRevokedManagedSSHCertificateHolders :many
-- Holders of a still-valid certificate for the machine that may no longer log in to it: the host refuses their key IDs
-- (bounded: one row per CLI session that got a certificate within the validity; the caller caps it).
SELECT h.user_id, h.cli_client_session_id FROM managed_ssh_certificate_holders h
JOIN user_machines m ON m.id = h.machine_id
LEFT JOIN cli_client_sessions session ON session.id = h.cli_client_session_id AND session.user_id = h.user_id
  AND session.state = 'active' AND session.revoked_at IS NULL
WHERE h.machine_id = sqlc.arg(machine_id) AND h.valid_before > sqlc.arg(now)
AND (session.id IS NULL OR NOT ((m.owner_team_id IS NULL AND m.user_id = h.user_id AND m.deleted_at IS NULL)
  OR machine_capability_allowed(h.user_id, m.id, 'managed_ssh') OR machine_management_allowed(h.user_id, m.id)))
ORDER BY h.cli_client_session_id
LIMIT sqlc.arg('limit');

-- name: DeleteExpiredManagedSSHCertificateHolders :exec
DELETE FROM managed_ssh_certificate_holders WHERE machine_id = sqlc.arg(machine_id) AND valid_before <= sqlc.arg(now);
