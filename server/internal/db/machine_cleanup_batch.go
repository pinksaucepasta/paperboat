package db

import (
	"context"
	"fmt"
	"time"
)

// batchArgs says which of ids, user and now a batch statement takes, always in that order as $1, $2, $3
// (user and now are skipped when unused, so a statement never has a parameter Postgres cannot type).
type batchArgs int

const (
	argsIDs batchArgs = iota
	argsIDsNow
	argsIDsUser
	argsIDsUserNow
)

// cleanupMachineBatchSteps are CleanupMachineMachineTx's statements for many machines at once ($1 = machine ids),
// in its order. Each table is written once per chunk instead of once per machine. The SSH host key children stay
// separate statements before their RESTRICT-referenced owners, as in the per-machine path.
// The per-machine ExpireAuthenticatedMachineInstallationPairingsForMachine is covered by the pairing expiry here.
var cleanupMachineBatchSteps = []struct {
	name, query string
	args        batchArgs
}{
	{"E2EE keys", `UPDATE account_e2ee_keys AS k SET revoked_at=coalesce(k.revoked_at,$3), revocation_reason=coalesce(k.revocation_reason,'machine_removed'), updated_at=$3
WHERE k.user_id=$2 AND k.revoked_at IS NULL AND (k.user_machine_id=ANY($1) OR k.cli_client_session_id IN (SELECT s.id FROM cli_client_sessions s WHERE s.user_machine_id=ANY($1) AND s.user_id=$2))`, argsIDsUserNow},
	{"CLI sessions", `UPDATE cli_client_sessions SET state='revoked', revoked_at=coalesce(revoked_at,$3), revocation_reason=coalesce(revocation_reason,'machine_removed'), version=version+1
WHERE user_machine_id=ANY($1) AND user_id=$2 AND state='active'`, argsIDsUserNow},
	{"CLI access tokens", `UPDATE cli_access_tokens AS t SET revoked_at=coalesce(t.revoked_at,$3) FROM cli_client_sessions s
WHERE t.cli_client_session_id=s.id AND s.user_machine_id=ANY($1) AND s.user_id=$2 AND t.revoked_at IS NULL`, argsIDsUserNow},
	{"CLI refresh tokens", `UPDATE cli_refresh_tokens AS t SET state='revoked', revoked_at=coalesce(t.revoked_at,$3) FROM cli_client_sessions s
WHERE t.cli_client_session_id=s.id AND s.user_machine_id=ANY($1) AND s.user_id=$2 AND t.state<>'revoked'`, argsIDsUserNow},
	{"peer enrollment requests", `UPDATE peer_endpoint_enrollment_requests SET state='revoked'
WHERE user_id=$2 AND state='pending' AND (endpoint_id=ANY($1) OR endpoint_id IN (SELECT s.id FROM cli_client_sessions s WHERE s.user_machine_id=ANY($1) AND s.user_id=$2))`, argsIDsUser},
	{"peer certificates", `UPDATE peer_endpoint_certificates AS c SET revoked_at=coalesce(c.revoked_at,$3),
revocation_reason=CASE WHEN c.revoked_at IS NULL THEN 'endpoint_removed' ELSE c.revocation_reason END
WHERE c.user_id=$2 AND c.revoked_at IS NULL AND (c.endpoint_id=ANY($1) OR c.endpoint_id IN (SELECT s.id FROM cli_client_sessions s WHERE s.user_machine_id=ANY($1) AND s.user_id=$2))`, argsIDsUserNow},
	{"pairings", `UPDATE user_machine_pairings SET state='expired', expires_at=least(expires_at,$2), installation_config_ciphertext=NULL, installation_config_nonce=NULL,
installation_config_consumed_at=NULL, installation_recovery_operation_key=NULL, updated_at=$2
WHERE user_machine_id=ANY($1) AND state IN ('pending','approved','consumed')`, argsIDsNow},
	{"pairing installations", `UPDATE user_machine_pairings SET installation_state='deleted', expires_at=least(expires_at,$2), bootstrap_token_hash=NULL, bootstrap_token_ciphertext=NULL, updated_at=$2
WHERE user_machine_id=ANY($1) AND installation_state IN ('awaiting_bootstrap','awaiting_approval','approved','material_issued','installing','connecting','ready','failed_retryable')`, argsIDsNow},
	{"diagnostic upload intents", `UPDATE diagnostic_upload_intents AS u SET state='expired' FROM cli_client_sessions s
WHERE u.cli_client_session_id=s.id AND s.user_machine_id=ANY($1) AND s.user_id=$2 AND u.state='pending'`, argsIDsUser},
	{"control sessions", `DELETE FROM machine_control_sessions WHERE machine_id=ANY($1)`, argsIDs},
	{"control renewals", `DELETE FROM machine_control_renewals WHERE machine_id=ANY($1)`, argsIDs},
	{"transfer destination defaults", `DELETE FROM user_transfer_destination_defaults WHERE user_id=$2 AND machine_id=ANY($1)`, argsIDsUser},
	{"terminal transfer destinations", `UPDATE user_machine_terminal_sessions SET transfer_destination_machine_id=NULL, version=version+1, updated_at=$2
WHERE transfer_destination_machine_id=ANY($1)`, argsIDsNow},
	{"terminal session operations", `DELETE FROM user_machine_terminal_session_operations WHERE user_machine_id=ANY($1)`, argsIDs},
	{"terminal sessions", `UPDATE user_machine_terminal_sessions SET desired_state='deleted', deleted_at=coalesce(deleted_at,$2), version=version+1, updated_at=$2
WHERE user_machine_id=ANY($1) AND deleted_at IS NULL`, argsIDsNow},
	{"availability operations", `DELETE FROM user_machine_availability_operations WHERE user_machine_id=ANY($1)`, argsIDs},
	{"update observations", `DELETE FROM user_machine_update_observations WHERE user_machine_id=ANY($1)`, argsIDs},
	{"maintenance approvals", `UPDATE user_machine_maintenance_approvals SET status='expired', decided_at=coalesce(decided_at,$2), updated_at=$2
WHERE user_machine_id=ANY($1) AND status IN ('pending','approved')`, argsIDsNow},
	{"SSH host keys", `DELETE FROM machine_ssh_host_keys WHERE user_machine_id=ANY($1)`, argsIDs},
	{"SSH host key sets", `DELETE FROM machine_ssh_host_key_sets WHERE user_machine_id=ANY($1)`, argsIDs},
	{"SSH host key owners", `DELETE FROM machine_ssh_host_key_owners WHERE user_machine_id=ANY($1)`, argsIDs},
	{"SSH targets", `DELETE FROM machine_ssh_targets WHERE user_machine_id=ANY($1)`, argsIDs},
}

// revokeEnvironmentBatchSteps are RevokeMachineEnvironmentTx's statements for many environments ($1 = ids, $2 = now).
var revokeEnvironmentBatchSteps = []struct{ name, query string }{
	{"environments", `UPDATE control_environments SET desired_state='revoked', desired_version=desired_version+1, revoked_at=coalesce(revoked_at,$2), updated_at=$2
WHERE id=ANY($1) AND desired_state<>'revoked'`},
	{"helpers", `UPDATE control_helpers SET state='revoked', revoked_at=coalesce(revoked_at,$2), updated_at=$2
WHERE environment_id=ANY($1) AND state IN ('pending','active') AND revoked_at IS NULL`},
	{"helper enrollments", `UPDATE control_helper_enrollments SET state='revoked', revoked_at=coalesce(revoked_at,$2)
WHERE environment_id=ANY($1) AND state='pending' AND revoked_at IS NULL`},
	{"connectors", `UPDATE control_connector_generations SET state='revoked', revoked_at=coalesce(revoked_at,$2), version=version+1, updated_at=$2
WHERE environment_id=ANY($1) AND state IN ('pending','admitted','draining') AND revoked_at IS NULL`},
	{"routes", `UPDATE control_routes SET desired_state=CASE WHEN applied_node_id IS NULL THEN 'detached' ELSE 'detaching' END,
desired_revision=desired_revision+1, version=version+1, updated_at=$2
WHERE environment_id=ANY($1) AND desired_state NOT IN ('detaching','detached')`},
	{"config credentials", `UPDATE control_config_credentials SET revoked_at=coalesce(revoked_at,$2) WHERE environment_id=ANY($1) AND revoked_at IS NULL`},
	{"config repository access", `UPDATE control_config_repository_access_operations SET state='revoked', revoked_at=coalesce(revoked_at,$2), updated_at=$2
WHERE environment_id=ANY($1) AND revoked_at IS NULL AND state IN ('pending','issued','uncertain')`},
	{"config repository leases", `UPDATE control_config_repository_lease_authority SET revoked_at=$2, version=version+1, updated_at=$2
WHERE environment_id=ANY($1) AND lease_id IS NOT NULL AND revoked_at IS NULL`},
}

// DeleteUserMachinesBatchTx deletes the given machines of one account, and fences their credentials, state and
// environments, with one statement per table for the whole batch: DeleteMachineTx's work without its per-machine
// round trips. The caller holds the machine row locks and writes the audit events. It is for one bounded chunk.
func DeleteUserMachinesBatchTx(ctx context.Context, tx *Tx, userID string, machineIDs, environmentIDs []string, now time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE user_machines SET state='deleted', online=false, seat_state='released', deleted_at=now(), updated_at=now(), version=version+1
WHERE id=ANY($1) AND user_id=$2 AND deleted_at IS NULL`, machineIDs, userID)
	if err != nil {
		return fmt.Errorf("delete machines: %w", err)
	}
	if int(tag.RowsAffected()) != len(machineIDs) {
		return fmt.Errorf("delete machines: %d of %d machines were still present", tag.RowsAffected(), len(machineIDs))
	}
	// Signed credentials need no helper call; the rest are left revoked for RetryPendingRevocations to deliver.
	if _, err := tx.Exec(ctx, `UPDATE user_machine_access_sessions AS a SET state='revoked', revoked_at=coalesce(a.revoked_at,now()), revocation_reason='user_machine_deleted', updated_at=now(),
helper_revoked_at=CASE WHEN a.credential_kind='control_plane_signed' THEN now() END
WHERE a.user_machine_id=ANY($1) AND a.state='active' AND (a.expires_at>now() OR a.credential_kind<>'control_plane_signed')`, machineIDs); err != nil {
		return fmt.Errorf("revoke access sessions: %w", err)
	}
	for _, step := range cleanupMachineBatchSteps {
		var args []any
		switch step.args {
		case argsIDs:
			args = []any{machineIDs}
		case argsIDsNow:
			args = []any{machineIDs, now}
		case argsIDsUser:
			args = []any{machineIDs, userID}
		default:
			args = []any{machineIDs, userID, now}
		}
		if _, err := tx.Exec(ctx, step.query, args...); err != nil {
			return fmt.Errorf("clean up machines (%s): %w", step.name, err)
		}
	}
	for _, step := range revokeEnvironmentBatchSteps {
		if _, err := tx.Exec(ctx, step.query, environmentIDs, now); err != nil {
			return fmt.Errorf("revoke machine environments (%s): %w", step.name, err)
		}
	}
	return nil
}
