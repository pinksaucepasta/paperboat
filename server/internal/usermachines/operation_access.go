package usermachines

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/admission"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

type operationAccess struct {
	claims mint.CredentialInput
	token  string
}

// prepareOperationAccess runs after authorization. An operation key serializes lookups
// and inserts of its credentials without queuing unrelated operations.
func (s *Service) prepareOperationAccess(ctx context.Context, tx *db.Tx, machine dbsqlc.UserMachine, in *dbsqlc.CreateUserMachineAccessSessionParams, operation *operationAccess) (bool, error) {
	claims := operation.claims
	claims.WorkspaceID = "personal"
	if scope, selected := workspace.FromContext(ctx); selected {
		if scope.TeamID != "" {
			claims.WorkspaceID = scope.TeamID
		}
	} else if machine.UserID != in.UserID {
		return false, ErrNotFound
	} else if machine.OwnerTeamID.Valid {
		claims.WorkspaceID = machine.OwnerTeamID.String
	}
	key := sha256.Sum256([]byte(strings.Join([]string{
		claims.CredentialClass, in.UserID, in.CLIClientSessionID, claims.SourceMachineID,
		in.UserMachineID, in.EnvironmentID, strconv.FormatInt(machine.InstallationGeneration, 10),
		claims.WorkspaceID, in.TeamID.String, in.OperationID,
	}, "\x00")))
	in.OperationKey = sql.NullString{String: hex.EncodeToString(key[:]), Valid: true}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,1885687003))`, in.OperationKey.String); err != nil {
		return false, err
	}
	claims.IssuedAt = s.now().UTC()
	claims.ExpiresAt = admission.Expiry(s.controlSigner, claims.IssuedAt)

	existing, err := tx.Queries().FindLiveOperationAccessSession(ctx, dbsqlc.FindLiveOperationAccessSessionParams{
		OperationKey: in.OperationKey, IssuedAt: claims.IssuedAt,
	})
	reused := err == nil
	if reused {
		claims.AssignmentID, claims.JTI, claims.ExpiresAt = existing.ID, existing.HelperTerminalSessionID.String, existing.ExpiresAt
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	} else {
		claims.AssignmentID, claims.JTI = newID("session"), newID("token")
		in.ID, in.HelperTerminalSessionID = claims.AssignmentID, claims.JTI
	}
	token, err := admission.Sign(s.controlSigner, claims)
	if err != nil {
		return false, err
	}
	in.ExpiresAt = claims.ExpiresAt
	operation.claims, operation.token = claims, token
	return reused, nil
}

// ReleaseOperationAccess ends the named exec or SSH operation and any admission
// renewed for it. The caller can release only its own CLI session's authority.
func (s *Service) ReleaseOperationAccess(ctx context.Context, userID, cliSessionID, machineID, operationID, capability string) error {
	if machineID == "" || !execOperationIDPattern.MatchString(operationID) || capability != "exec" && capability != "managed_ssh" {
		return ErrExecOperationInvalid
	}
	return s.db.Queries().ReleaseOperationAccessSessions(ctx, dbsqlc.ReleaseOperationAccessSessionsParams{
		UserID: userID, CLIClientSessionID: cliSessionID, MachineID: machineID, OperationID: operationID, Capability: capability,
	})
}
