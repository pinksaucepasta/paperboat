package previewtunnelstore

import (
	"context"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// LazyLeaseAuthority is an internal activation binding, never a general preview
// management grant. Running activation requires a live authorized waiter; a
// renewal must name the exact preview already attached to that activation.
type LazyLeaseAuthority struct {
	AccountID, MachineID, OwnerSessionID     string
	OwnerSessionKind                         string
	TargetScheme, TargetAddress, Endpoint    string
	ActivationID, PreviewID                  string
	PolicyID                                 string
	PolicyGeneration, InstallationGeneration int64
}

const lazyLeaseAuthoritySQL = `SELECT m.owner_team_id IS NOT NULL
 FROM paperboat.lazy_access_policies p
 JOIN paperboat.lazy_activations a ON a.policy_id=p.id AND a.policy_generation=p.generation
 JOIN paperboat.lazy_runtime_owners r ON r.machine_id=p.machine_id AND r.account_id=p.account_id
 JOIN paperboat.user_machines m ON m.id=p.machine_id AND m.user_id=p.account_id
 WHERE p.account_id=$1 AND p.machine_id=$2 AND a.owner_session_id=$3
 AND p.target_scheme=$4 AND p.target_address=$5 AND 'https://'||p.hostname=$6
 AND p.access_mode IN ('private','team') AND p.deleted_at IS NULL AND p.expires_at>now()
 AND m.installation_generation=p.installation_generation AND m.environment_id=p.environment_id
 AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.online AND m.state='online'
 AND m.seat_state='occupied'
 AND r.installation_generation=p.installation_generation AND r.boot_id=a.boot_id AND r.expires_at>now()
 AND ($9='' OR p.id=$9) AND ($10=0 OR p.generation=$10) AND ($11=0 OR p.installation_generation=$11)
 AND ((a.state='running' AND (a.activation_id=$7 OR ($8<>'' AND EXISTS(SELECT 1 FROM paperboat.operations op WHERE op.account_id=p.account_id AND op.resource_id=$8 AND op.resource_kind='preview_lease' AND op.operation_type='preview.create' AND op.idempotency_key=a.activation_id))) AND a.deadline>now()
       AND EXISTS(SELECT 1 FROM paperboat.lazy_activation_waiters w WHERE w.activation_id=a.activation_id AND w.expires_at>now()))
   OR (a.state='ready' AND a.preview_id=$8 AND $8<>''))`

func lazyAuthorityArgs(b LazyLeaseAuthority) []any {
	return []any{b.AccountID, b.MachineID, b.OwnerSessionID, b.TargetScheme, b.TargetAddress, b.Endpoint, b.ActivationID, b.PreviewID, b.PolicyID, b.PolicyGeneration, b.InstallationGeneration}
}
func (s *Store) VerifyLazyPreviewLeaseV1(ctx context.Context, b LazyLeaseAuthority) (bool, error) {
	if b.OwnerSessionKind != "lazy_runtime" {
		return false, ErrOwnerNotFound
	}
	return verifyPreviewMachineAuthority(s.db.Pool().QueryRow(ctx, lazyLeaseAuthoritySQL, lazyAuthorityArgs(b)...), "private")
}
func VerifyLazyPreviewLeaseTx(ctx context.Context, tx *db.Tx, b LazyLeaseAuthority) (bool, error) {
	if b.OwnerSessionKind != "lazy_runtime" {
		return false, ErrOwnerNotFound
	}
	return verifyPreviewMachineAuthority(tx.QueryRow(ctx, lazyLeaseAuthoritySQL, lazyAuthorityArgs(b)...), "private")
}
