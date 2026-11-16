package db

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

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

// FenceRemovedMachineVaultTx fences delivery so removed and still-connected devices stop receiving ENV; it never requires key rotation.
// With a machine it fences only the sources that machine could have read (it received a layer
// from them, or it is the source), so removing a device that never received ENV leaves every
// other machine's delivery untouched. An empty machine (a revoked CLI session) fences the whole account.
func FenceRemovedMachineVaultTx(ctx context.Context, tx *Tx, account, machine string) error {
	if err := LockTeamWrites(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE environment_vault_layer_fences f SET generation=generation+1 WHERE f.owner_kind='personal' AND f.owner_id=$1 AND ($2='' OR f.source_machine_id=$2 OR EXISTS(SELECT 1 FROM environment_vault_layers l WHERE l.workspace_id=f.workspace_id AND l.owner_kind=f.owner_kind AND l.owner_id=f.owner_id AND l.source_machine_id=f.source_machine_id AND l.machine_id=$2))`, account, machine); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `WITH affected AS (SELECT t.team_id FROM environment_vault_teams t WHERE EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=t.team_id AND m.account_id=$1 AND m.active AND (($2='' AND (EXISTS(SELECT 1 FROM environment_vault_team_members e WHERE e.team_id=t.team_id AND e.account_id=$1 AND e.grant_epoch=t.key_epoch) OR EXISTS(SELECT 1 FROM environment_vault_layers l JOIN user_machines recipient ON recipient.id=l.machine_id WHERE l.owner_kind='team' AND l.owner_id=t.team_id AND recipient.user_id=$1))) OR ($2<>'' AND EXISTS(SELECT 1 FROM environment_vault_layers l WHERE l.owner_kind='team' AND l.owner_id=t.team_id AND (l.machine_id=$2 OR l.source_machine_id=$2)))))), advanced AS (UPDATE teams SET generation=generation+1 WHERE team_id IN(SELECT team_id FROM affected)) UPDATE environment_vault_layer_fences f SET generation=generation+1 WHERE f.owner_kind='team' AND f.owner_id IN(SELECT team_id FROM affected) AND ($2='' OR f.source_machine_id=$2 OR EXISTS(SELECT 1 FROM environment_vault_layers l WHERE l.workspace_id=f.workspace_id AND l.owner_kind=f.owner_kind AND l.owner_id=f.owner_id AND l.source_machine_id=f.source_machine_id AND l.machine_id=$2))`, account, machine)
	return err
}
