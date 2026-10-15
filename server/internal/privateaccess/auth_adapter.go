package privateaccess

import (
	"context"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

type SQLMachineStateVerifier struct{ db *db.DB }

func NewSQLMachineStateVerifier(database *db.DB) (*SQLMachineStateVerifier, error) {
	if database == nil || database.Pool() == nil {
		return nil, fmt.Errorf("%w: machine state database is not open", ErrInvalid)
	}
	return &SQLMachineStateVerifier{db: database}, nil
}

func (v *SQLMachineStateVerifier) VerifyCurrentMachine(ctx context.Context, identity Identity, now time.Time) error {
	return v.verifyMachine(ctx, identity, now, false)
}
func (v *SQLMachineStateVerifier) VerifyAdmittedMachine(ctx context.Context, identity Identity, now time.Time) error {
	return v.verifyMachine(ctx, identity, now, true)
}
func (v *SQLMachineStateVerifier) verifyMachine(ctx context.Context, identity Identity, now time.Time, admitted bool) error {
	if v == nil || v.db == nil || ctx == nil || identity.InstallationGeneration == 0 {
		return ErrIdentityUnavailable
	}
	var ok bool
	err := v.db.Pool().QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM user_machines machines
		JOIN machine_control_sessions control ON control.machine_id = machines.id
		WHERE machines.id=$1 AND machines.user_id=$2 AND machines.installation_generation=$3
		  AND control.installation_generation=machines.installation_generation
		  AND ($6 OR (control.credential_jti=$4 AND control.expires_at > $5))
		  AND (($6 AND machines.state IN ('online','offline')) OR (machines.state='online' AND machines.online))
		  AND machines.deleted_at IS NULL AND machines.revoked_at IS NULL
	)`, identity.MachineID, identity.AccountID, int64(identity.InstallationGeneration), identity.SessionID, now.UTC(), admitted).Scan(&ok)
	if err != nil {
		return fmt.Errorf("%w: verify current machine: %w", ErrIdentityUnavailable, err)
	}
	if !ok {
		return newDenied(ReasonMachineRevoked)
	}
	return nil
}
