package db

import (
	"context"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// DeleteUserMachinesBatchTx deletes the given machines of one account, and fences their credentials, state and
// environments, with one statement per table for the whole batch: DeleteMachineTx's work without its per-machine
// round trips (queries/machine_cleanup_batch.sql). The caller holds the machine row locks and writes the audit
// events. It is for one bounded chunk. The statements keep CleanupMachineMachineTx's order; the per-machine
// ExpireAuthenticatedMachineInstallationPairingsForMachine is covered by the pairing expiry (BatchPairings).
func DeleteUserMachinesBatchTx(ctx context.Context, tx *Tx, userID string, ids, envIDs []string, now time.Time) error {
	q := tx.Queries()
	changed, err := q.BatchDeleteUserMachines(ctx, dbsqlc.BatchDeleteUserMachinesParams{MachineIds: ids, UserID: userID})
	if err != nil {
		return fmt.Errorf("delete machines: %w", err)
	}
	if int(changed) != len(ids) {
		return fmt.Errorf("delete machines: %d of %d machines were still present", changed, len(ids))
	}
	// Signed credentials need no helper call; the rest are left revoked for RetryPendingRevocations to deliver.
	if err := q.BatchRevokeUserMachineAccessSessions(ctx, ids); err != nil {
		return fmt.Errorf("revoke access sessions: %w", err)
	}
	steps := []struct {
		name string
		run  func() error
	}{
		{"E2EE keys", func() error {
			return q.BatchE2eeKeys(ctx, dbsqlc.BatchE2eeKeysParams{MachineIds: ids, UserID: userID, Now: now})
		}},
		{"CLI sessions", func() error {
			return q.BatchCliSessions(ctx, dbsqlc.BatchCliSessionsParams{MachineIds: ids, UserID: userID, Now: now})
		}},
		{"CLI access tokens", func() error {
			return q.BatchCliAccessTokens(ctx, dbsqlc.BatchCliAccessTokensParams{MachineIds: ids, UserID: userID, Now: now})
		}},
		{"CLI refresh tokens", func() error {
			return q.BatchCliRefreshTokens(ctx, dbsqlc.BatchCliRefreshTokensParams{MachineIds: ids, UserID: userID, Now: now})
		}},
		{"peer enrollment requests", func() error {
			return q.BatchPeerEnrollmentRequests(ctx, dbsqlc.BatchPeerEnrollmentRequestsParams{MachineIds: ids, UserID: userID})
		}},
		{"peer certificates", func() error {
			return q.BatchPeerCertificates(ctx, dbsqlc.BatchPeerCertificatesParams{MachineIds: ids, UserID: userID, Now: now})
		}},
		{"pairings", func() error { return q.BatchPairings(ctx, dbsqlc.BatchPairingsParams{MachineIds: ids, Now: now}) }},
		{"pairing installations", func() error {
			return q.BatchPairingInstallations(ctx, dbsqlc.BatchPairingInstallationsParams{MachineIds: ids, Now: now})
		}},
		{"diagnostic upload intents", func() error {
			return q.BatchDiagnosticUploadIntents(ctx, dbsqlc.BatchDiagnosticUploadIntentsParams{MachineIds: ids, UserID: userID})
		}},
		{"control sessions", func() error { return q.BatchControlSessions(ctx, ids) }},
		{"control renewals", func() error { return q.BatchControlRenewals(ctx, ids) }},
		{"transfer destination defaults", func() error {
			return q.BatchTransferDestinationDefaults(ctx, dbsqlc.BatchTransferDestinationDefaultsParams{MachineIds: ids, UserID: userID})
		}},
		{"terminal transfer destinations", func() error {
			return q.BatchTerminalTransferDestinations(ctx, dbsqlc.BatchTerminalTransferDestinationsParams{MachineIds: ids, Now: now})
		}},
		{"terminal session operations", func() error { return q.BatchTerminalSessionOperations(ctx, ids) }},
		{"terminal sessions", func() error {
			return q.BatchTerminalSessions(ctx, dbsqlc.BatchTerminalSessionsParams{MachineIds: ids, Now: now})
		}},
		{"availability operations", func() error { return q.BatchAvailabilityOperations(ctx, ids) }},
		{"update observations", func() error { return q.BatchUpdateObservations(ctx, ids) }},
		{"maintenance approvals", func() error {
			return q.BatchMaintenanceApprovals(ctx, dbsqlc.BatchMaintenanceApprovalsParams{MachineIds: ids, Now: now})
		}},
		{"SSH host keys", func() error { return q.BatchSshHostKeys(ctx, ids) }},
		{"SSH host key sets", func() error { return q.BatchSshHostKeySets(ctx, ids) }},
		{"SSH host key owners", func() error { return q.BatchSshHostKeyOwners(ctx, ids) }},
		{"SSH targets", func() error { return q.BatchSshTargets(ctx, ids) }},
		{"environment environments", func() error {
			return q.BatchRevokeEnvEnvironments(ctx, dbsqlc.BatchRevokeEnvEnvironmentsParams{EnvironmentIds: envIDs, Now: now})
		}},
		{"environment helpers", func() error {
			return q.BatchRevokeEnvHelpers(ctx, dbsqlc.BatchRevokeEnvHelpersParams{EnvironmentIds: envIDs, Now: now})
		}},
		{"environment helper enrollments", func() error {
			return q.BatchRevokeEnvHelperEnrollments(ctx, dbsqlc.BatchRevokeEnvHelperEnrollmentsParams{EnvironmentIds: envIDs, Now: now})
		}},
		{"environment connectors", func() error {
			return q.BatchRevokeEnvConnectors(ctx, dbsqlc.BatchRevokeEnvConnectorsParams{EnvironmentIds: envIDs, Now: now})
		}},
		{"environment routes", func() error {
			return q.BatchRevokeEnvRoutes(ctx, dbsqlc.BatchRevokeEnvRoutesParams{EnvironmentIds: envIDs, Now: now})
		}},
		{"environment config credentials", func() error {
			return q.BatchRevokeEnvConfigCredentials(ctx, dbsqlc.BatchRevokeEnvConfigCredentialsParams{EnvironmentIds: envIDs, Now: now})
		}},
		{"environment config repository access", func() error {
			return q.BatchRevokeEnvConfigRepositoryAccess(ctx, dbsqlc.BatchRevokeEnvConfigRepositoryAccessParams{EnvironmentIds: envIDs, Now: now})
		}},
		{"environment config repository leases", func() error {
			return q.BatchRevokeEnvConfigRepositoryLeases(ctx, dbsqlc.BatchRevokeEnvConfigRepositoryLeasesParams{EnvironmentIds: envIDs, Now: now})
		}},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			return fmt.Errorf("clean up machines (%s): %w", step.name, err)
		}
	}
	return nil
}
