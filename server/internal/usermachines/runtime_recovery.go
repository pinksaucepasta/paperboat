package usermachines

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
)

// runtimeRecoveryMaterial returns only the existing installation identity. The
// running profile owns CLI refresh; replaying installation tokens can revoke it.
func (s *Service) runtimeRecoveryMaterial(ctx context.Context, verifier, publicKey string, material json.RawMessage) (json.RawMessage, error) {
	binding, err := authenticatedMachineInstallationMaterialBindingFromPlaintext(material)
	if err != nil || publicKey == "" {
		return nil, ErrInstallationUnavailable
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(material, &fields) != nil {
		return nil, ErrInstallationUnavailable
	}
	var session installationClientSession
	if json.Unmarshal(fields["client_session"], &session) != nil || session.SessionID == "" {
		return nil, ErrInstallationUnavailable
	}
	hash := sha256.Sum256([]byte(verifier))
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		pairing, err := tx.Queries().GetUserMachinePairingForVerifier(ctx, hash[:])
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInstallationUnavailable
			}
			return err
		}
		if pairing.State != "consumed" || !pairing.ApprovedByUserID.Valid || pairing.PublicIdentityKey.String != publicKey || pairing.UserMachineID.String != binding.MachineID {
			return ErrInstallationUnavailable
		}
		row, err := tx.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: binding.MachineID, UserID: pairing.ApprovedByUserID.String})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInstallationUnavailable
			}
			return err
		}
		if row.EnvironmentID != binding.EnvironmentID || row.PublicIdentityKey.String != publicKey || row.InstallationGeneration != binding.Generation || row.SeatState != "occupied" || row.State == "revoked" || row.State == "deleted" {
			return ErrInstallationUnavailable
		}
		active, err := tx.Queries().GetActiveClientSessionForUserMachine(ctx, dbsqlc.GetActiveClientSessionForUserMachineParams{ID: session.SessionID, UserID: pairing.ApprovedByUserID.String, UserMachineID: sql.NullString{String: binding.MachineID, Valid: true}})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInstallationUnavailable
			}
			return err
		}
		if active.ClientID != s.cliClientID {
			return ErrInstallationUnavailable
		}
		helper, err := tx.Queries().GetActiveControlHelperForEnvironment(ctx, binding.EnvironmentID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInstallationUnavailable
			}
			return err
		}
		if helper.ID != binding.HelperID {
			return ErrInstallationUnavailable
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	delete(fields, "client_session")
	delete(fields, "enrollment_id")
	delete(fields, "enrollment_credential")
	fields["reuse_identity"] = json.RawMessage("true")
	return json.Marshal(fields)
}
