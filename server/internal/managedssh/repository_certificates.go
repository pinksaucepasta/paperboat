package managedssh

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// CertificateAuthority authorizes issuing for the exact machine (live CLI session, managed SSH allowed, target set),
// records the session as a holder until request.ValidBefore, and returns the target OS user and the account's signing CA
// (the holder record is what lets the host refuse its key ID if the session later loses access).
func (r *SQLRepository) CertificateAuthority(ctx context.Context, request IssueCertificateRequest, candidate NewUserCA) (CertificateAuthorization, error) {
	var result CertificateAuthorization
	err := r.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		q := tx.Queries()
		row, err := q.ResolveMachineSSHCertificateAuthority(ctx, dbsqlc.ResolveMachineSSHCertificateAuthorityParams{CLIClientSessionID: request.CLIClientSessionID, UserID: request.UserID, MachineID: request.UserMachineID, MachineGeneration: int64(request.MachineGeneration)})
		if err != nil {
			return authorityError(err)
		}
		result.OSUser = row.OsUser
		if err := r.ensureUserCA(ctx, tx, row.AccountID, candidate, request.Now); err != nil {
			return err
		}
		active, err := q.GetActiveManagedSSHUserCA(ctx, row.AccountID)
		if err != nil {
			return err
		}
		result.CA = userCAFromRow(active)
		if err := q.RecordManagedSSHCertificateHolder(ctx, dbsqlc.RecordManagedSSHCertificateHolderParams{MachineID: request.UserMachineID, CLIClientSessionID: request.CLIClientSessionID, UserID: request.UserID, ValidBefore: request.ValidBefore}); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

// revokedKeyIDs are the key IDs of still-valid certificates for the machine whose holders may no longer log in to it.
func revokedKeyIDs(ctx context.Context, q *dbsqlc.Queries, machineID string, now time.Time) ([]string, error) {
	rows, err := q.ListRevokedManagedSSHCertificateHolders(ctx, dbsqlc.ListRevokedManagedSSHCertificateHoldersParams{MachineID: machineID, Now: now})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(rows))
	for index, row := range rows {
		ids[index] = CertificateKeyID(row.UserID, row.CLIClientSessionID, machineID)
	}
	return ids, nil
}

// UserCAs authorizes the machine itself (its owner account and generation) and returns every CA it must trust and
// the key IDs it must refuse. Holders whose certificates expired are dropped here (the table stays bounded).
func (r *SQLRepository) UserCAs(ctx context.Context, request UserCAsRequest, candidate NewUserCA) ([]UserCA, []string, error) {
	var result []UserCA
	var revoked []string
	// Read committed behind the machine's row lock: the account's first CA is one conditional insert (unique per state,
	// DO NOTHING), and every other write is this machine's own row, so a fetch that queues behind a config write to
	// its machine beats a 40001 replay (prod 2026-11-20).
	err := r.store.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		result, revoked = nil, nil
		q := tx.Queries()
		machine, err := q.ResolveMachineSSHHostKeyAuthorityForUpdate(ctx, dbsqlc.ResolveMachineSSHHostKeyAuthorityForUpdateParams{UserMachineID: request.UserMachineID, UserID: request.ActorUserID, MachineGeneration: int64(request.MachineGeneration)})
		if err != nil {
			return authorityError(err)
		}
		if err := r.ensureUserCA(ctx, tx, machine.UserID, candidate, request.Now); err != nil {
			return err
		}
		if result, err = listUserCAs(ctx, q, machine.UserID); err != nil {
			return err
		}
		// Break-glass reads this to see which hosts hold the CA set it published (breakglass pickup).
		if _, err := tx.Exec(ctx, `INSERT INTO paperboat.managed_ssh_ca_fetches (machine_id, fetched_at) VALUES ($1, $2)
			ON CONFLICT (machine_id) DO UPDATE SET fetched_at = EXCLUDED.fetched_at`, request.UserMachineID, request.Now); err != nil {
			return err
		}
		if err := q.DeleteExpiredManagedSSHCertificateHolders(ctx, dbsqlc.DeleteExpiredManagedSSHCertificateHoldersParams{MachineID: request.UserMachineID, Now: request.Now}); err != nil {
			return err
		}
		revoked, err = revokedKeyIDs(ctx, q, request.UserMachineID, request.Now)
		return err
	})
	return result, revoked, err
}

func (r *SQLRepository) RotateUserCA(ctx context.Context, request RotateUserCARequest, candidate NewUserCA) ([]UserCA, error) {
	var result []UserCA
	err := r.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		q := tx.Queries()
		cas, err := listUserCAs(ctx, q, request.AccountID)
		if err != nil {
			return err
		}
		byState := make(map[string]UserCA, len(cas))
		for _, ca := range cas {
			byState[ca.State] = ca
		}
		step := ""
		retiring, hasRetiring := byState["retiring"]
		pending, hasPending := byState["pending"]
		_, hasActive := byState["active"]
		switch {
		case !hasActive && !hasPending:
			return ErrUnavailable
		case hasRetiring && request.Now.Sub(retiring.StateChangedAt) >= request.Validity:
			step = "retired"
			err = q.DeleteManagedSSHUserCA(ctx, dbsqlc.DeleteManagedSSHUserCAParams{AccountID: request.AccountID, State: "retiring"})
		case hasPending && !hasRetiring && request.Now.Sub(pending.StateChangedAt) >= request.Propagation:
			step = "promoted"
			err = q.SetManagedSSHUserCAState(ctx, dbsqlc.SetManagedSSHUserCAStateParams{NewState: "retiring", Now: request.Now, AccountID: request.AccountID, OldState: "active"})
			if err == nil {
				err = q.SetManagedSSHUserCAState(ctx, dbsqlc.SetManagedSSHUserCAStateParams{NewState: "active", Now: request.Now, AccountID: request.AccountID, OldState: "pending"})
			}
		case !hasPending && !hasRetiring:
			step = "added"
			err = q.CreatePendingManagedSSHUserCA(ctx, dbsqlc.CreatePendingManagedSSHUserCAParams{ID: candidate.ID, AccountID: request.AccountID, PublicKey: candidate.PublicKey, PrivateKeyCiphertext: candidate.PrivateKeyCiphertext, Now: request.Now})
		}
		if err != nil {
			return err
		}
		if step != "" {
			if err := r.audit.WriteTx(ctx, tx, audit.Event{ActorType: audit.ActorAdmin, EventType: "managed_ssh.user_ca_rotation", ResourceType: "managed_ssh_user_ca", ResourceID: request.AccountID, Metadata: map[string]any{"step": step}}); err != nil {
				return err
			}
		}
		result, err = listUserCAs(ctx, q, request.AccountID)
		return err
	})
	return result, err
}

func (r *SQLRepository) ensureUserCA(ctx context.Context, tx *db.Tx, accountID string, candidate NewUserCA, now time.Time) error {
	return tx.Queries().CreateFirstManagedSSHUserCA(ctx, dbsqlc.CreateFirstManagedSSHUserCAParams{ID: candidate.ID, AccountID: accountID, PublicKey: candidate.PublicKey, PrivateKeyCiphertext: candidate.PrivateKeyCiphertext, Now: now})
}

func listUserCAs(ctx context.Context, q *dbsqlc.Queries, accountID string) ([]UserCA, error) {
	rows, err := q.ListManagedSSHUserCAs(ctx, accountID)
	if err != nil {
		return nil, err
	}
	result := make([]UserCA, len(rows))
	for index, row := range rows {
		result[index] = userCAFromRow(row)
	}
	return result, nil
}

func userCAFromRow(row dbsqlc.ManagedSshUserCa) UserCA {
	return UserCA{ID: row.ID, AccountID: row.AccountID, State: row.State, PublicKey: row.PublicKey, PrivateKeyCiphertext: row.PrivateKeyCiphertext, CreatedAt: row.CreatedAt, StateChangedAt: row.StateChangedAt}
}
