package db

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// RevokeMachinePeerCredentialsTx ends a machine's CLI sessions and peer authority (its own and those sessions'
// certificates). Both bump the peer identities of the owner's other machines through triggers
// (advance_peer_network_for_cli_session, advance_peer_network_for_certificate), rows their configuration requests
// update all the time, so removals run this first at READ COMMITTED, where the bumps wait for those writers instead of
// aborting; CleanupMachineMachineTx then finds nothing left of it to write. The caller holds the machine row lock.
func RevokeMachinePeerCredentialsTx(ctx context.Context, tx *Tx, userID, userMachineID string, now time.Time) error {
	revocationTime := sql.NullTime{Time: now, Valid: true}
	if _, err := tx.Queries().RevokeCLIClientSessionsForUserMachine(ctx, dbsqlc.RevokeCLIClientSessionsForUserMachineParams{
		RevocationTime: revocationTime, RevocationCause: sql.NullString{String: "machine_removed", Valid: true},
		TargetMachineID: sql.NullString{String: userMachineID, Valid: true}, TargetUserID: userID,
	}); err != nil {
		return err
	}
	_, err := tx.Queries().RevokeUserMachinePeerAuthority(ctx, dbsqlc.RevokeUserMachinePeerAuthorityParams{
		TargetMachineID: userMachineID, TargetUserID: userID, RevocationTime: revocationTime,
	})
	return err
}

// CleanupMachineMachineTx fences machine credentials and state in the owning
// deletion transaction. Personal and team lifecycle paths share this cleanup.
func CleanupMachineMachineTx(ctx context.Context, tx *Tx, userID, userMachineID string, now time.Time) error {
	queries := tx.Queries()
	machineID := sql.NullString{String: userMachineID, Valid: true}
	revocationTime := sql.NullTime{Time: now, Valid: true}

	if _, err := queries.RevokeUserMachineE2EEKeys(ctx, dbsqlc.RevokeUserMachineE2EEKeysParams{
		RevocationTime: now, TargetUserID: userID, TargetMachineID: machineID,
	}); err != nil {
		return err
	}
	if _, err := queries.RevokeCLIClientSessionsForUserMachine(ctx, dbsqlc.RevokeCLIClientSessionsForUserMachineParams{
		RevocationTime: revocationTime, RevocationCause: sql.NullString{String: "machine_removed", Valid: true}, TargetMachineID: machineID, TargetUserID: userID,
	}); err != nil {
		return err
	}
	if _, err := queries.RevokeCLIClientAccessTokensForUserMachine(ctx, dbsqlc.RevokeCLIClientAccessTokensForUserMachineParams{
		RevocationTime: revocationTime, TargetMachineID: machineID, TargetUserID: userID,
	}); err != nil {
		return err
	}
	if _, err := queries.RevokeCLIClientRefreshTokensForUserMachine(ctx, dbsqlc.RevokeCLIClientRefreshTokensForUserMachineParams{
		RevocationTime: revocationTime, TargetMachineID: machineID, TargetUserID: userID,
	}); err != nil {
		return err
	}
	if _, err := queries.RevokeUserMachinePeerAuthority(ctx, dbsqlc.RevokeUserMachinePeerAuthorityParams{
		TargetMachineID: userMachineID, TargetUserID: userID, RevocationTime: revocationTime,
	}); err != nil {
		return err
	}
	if _, err := queries.ExpireUserMachinePairings(ctx, dbsqlc.ExpireUserMachinePairingsParams{ExpiredAt: now, TargetMachineID: machineID}); err != nil {
		return err
	}
	if _, err := queries.DeletePairingInstallations(ctx, dbsqlc.DeletePairingInstallationsParams{ExpiredAt: now, TargetMachineID: machineID}); err != nil {
		return err
	}
	if _, err := queries.ExpireUserMachineDiagnosticUploadIntents(ctx, dbsqlc.ExpireUserMachineDiagnosticUploadIntentsParams{TargetMachineID: machineID, TargetUserID: userID}); err != nil {
		return err
	}
	if _, err := queries.DeleteUserMachineControlSessions(ctx, userMachineID); err != nil {
		return err
	}
	if _, err := queries.DeleteUserMachineControlRenewals(ctx, userMachineID); err != nil {
		return err
	}
	if _, err := queries.DeleteUserMachineTransferDestinationDefault(ctx, dbsqlc.DeleteUserMachineTransferDestinationDefaultParams{TargetUserID: userID, TargetMachineID: userMachineID}); err != nil {
		return err
	}
	if _, err := queries.ClearUserMachineTerminalTransferDestinationsForMachine(ctx, dbsqlc.ClearUserMachineTerminalTransferDestinationsForMachineParams{ExpiredAt: now, TargetMachineID: machineID}); err != nil {
		return err
	}
	if _, err := queries.DeleteUserMachineTerminalSessionOperations(ctx, userMachineID); err != nil {
		return err
	}
	if _, err := queries.DeleteUserMachineTerminalSessions(ctx, dbsqlc.DeleteUserMachineTerminalSessionsParams{ExpiredAt: now, TargetMachineID: userMachineID}); err != nil {
		return err
	}
	if _, err := queries.DeleteUserMachineAvailabilityOperations(ctx, userMachineID); err != nil {
		return err
	}
	if _, err := queries.DeleteUserMachineUpdateObservation(ctx, userMachineID); err != nil {
		return err
	}
	if _, err := queries.ExpireUserMachineMaintenanceApprovals(ctx, dbsqlc.ExpireUserMachineMaintenanceApprovalsParams{ExpiredAt: now, TargetMachineID: userMachineID}); err != nil {
		return err
	}
	if _, err := queries.DeleteUserMachineSSHHostKeys(ctx, userMachineID); err != nil {
		return err
	}
	if _, err := queries.DeleteUserMachineSSHHostKeySets(ctx, userMachineID); err != nil {
		return err
	}
	if _, err := queries.DeleteUserMachineSSHHostKeyOwners(ctx, userMachineID); err != nil {
		return err
	}
	if _, err := queries.DeleteUserMachineSSHTarget(ctx, userMachineID); err != nil {
		return err
	}
	return nil
}

func RevokeMachineEnvironmentTx(ctx context.Context, tx *Tx, environmentID string, now time.Time) error {
	environment, err := tx.Queries().GetControlEnvironment(ctx, environmentID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && environment.DesiredState != "revoked" {
		if _, err := tx.Queries().UpdateControlEnvironmentDesiredState(ctx, dbsqlc.UpdateControlEnvironmentDesiredStateParams{DesiredState: "revoked", Now: now, ID: environmentID, ExpectedVersion: environment.DesiredVersion}); err != nil {
			return err
		}
	}
	if _, err := tx.Queries().RevokeControlHelpersForEnvironment(ctx, dbsqlc.RevokeControlHelpersForEnvironmentParams{RevokedAt: now, EnvironmentID: environmentID}); err != nil {
		return err
	}
	if _, err := tx.Queries().RevokeControlHelperEnrollmentsForEnvironment(ctx, dbsqlc.RevokeControlHelperEnrollmentsForEnvironmentParams{RevokedAt: sql.NullTime{Time: now, Valid: true}, EnvironmentID: environmentID}); err != nil {
		return err
	}
	if _, err := tx.Queries().RevokeControlConnectorForEnvironment(ctx, dbsqlc.RevokeControlConnectorForEnvironmentParams{RevokedAt: now, EnvironmentID: environmentID}); err != nil {
		return err
	}
	if _, err := tx.Queries().RevokeControlRoutesForEnvironment(ctx, dbsqlc.RevokeControlRoutesForEnvironmentParams{EnvironmentID: environmentID, Now: now}); err != nil {
		return err
	}
	if _, err = tx.Queries().RevokeControlConfigCredentialsForEnvironment(ctx, dbsqlc.RevokeControlConfigCredentialsForEnvironmentParams{EnvironmentID: environmentID, RevokedAt: sql.NullTime{Time: now, Valid: true}}); err != nil {
		return err
	}
	if _, err = tx.Queries().RevokeControlConfigRepositoryAccessForEnvironment(ctx, dbsqlc.RevokeControlConfigRepositoryAccessForEnvironmentParams{EnvironmentID: environmentID, Now: now}); err != nil {
		return err
	}
	if _, err = tx.Queries().RevokeControlConfigRepositoryLeasesForEnvironment(ctx, dbsqlc.RevokeControlConfigRepositoryLeasesForEnvironmentParams{EnvironmentID: sql.NullString{String: environmentID, Valid: true}, Now: sql.NullTime{Time: now, Valid: true}}); err != nil {
		return err
	}
	return nil
}
