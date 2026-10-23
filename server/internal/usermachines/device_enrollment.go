package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// DeviceEnrollmentKey is the idempotency key of the enrollment a device login mints, so a
// retried poll of the same approved grant hands the device the same one-shot token.
func DeviceEnrollmentKey(grantID string) string { return "device_grant:" + grantID }

// IssueDeviceEnrollment mints the normal one-shot enrollment for an approved device login.
// workspaceTeamID is empty for Personal.
func (s *Service) IssueDeviceEnrollment(ctx context.Context, userID, grantID, workspaceTeamID, shell string) (EnrollmentStart, error) {
	return s.StartEnrollmentWithOptions(ctx, userID, DeviceEnrollmentKey(grantID), EnrollmentOptions{Shell: shell, WorkspaceTeamID: workspaceTeamID})
}

// EnrolledMachineWorkspaces names where an already enrolled machine of this account lives:
// the team that owns it, or Personal plus the teams it is shared with. ok is false when the
// machine does not exist or belongs to someone else.
func (s *Service) EnrolledMachineWorkspaces(ctx context.Context, userID, machineID string) (alias string, workspaces []string, ok bool, err error) {
	var ownerTeam sql.NullString
	err = s.db.SQL().QueryRowContext(ctx, `SELECT alias, owner_team_id FROM user_machines WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL AND deleted_at IS NULL`, strings.TrimSpace(machineID), userID).Scan(&alias, &ownerTeam)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	if ownerTeam.Valid {
		return alias, []string{ownerTeam.String}, true, nil
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT b.team_id FROM team_resource_bindings b WHERE b.resource_kind='machine' AND b.resource_id=$1 AND b.active ORDER BY b.team_id`, machineID)
	if err != nil {
		return "", nil, false, err
	}
	defer rows.Close()
	workspaces = []string{"personal"}
	for rows.Next() {
		var team string
		if err = rows.Scan(&team); err != nil {
			return "", nil, false, err
		}
		workspaces = append(workspaces, team)
	}
	return alias, workspaces, true, rows.Err()
}

// placeEnrolledMachine puts the machine into the team the enrollment was minted for, in the
// same transaction that creates it. A Personal enrollment (no team) needs nothing.
func placeEnrolledMachine(ctx context.Context, tx *db.Tx, userID string, pairing dbsqlc.UserMachinePairing, machineID string) error {
	if !pairing.WorkspaceTeamID.Valid {
		return nil
	}
	_, err := teams.ShareEnrolledMachineTx(ctx, tx, userID, pairing.WorkspaceTeamID.String, machineID)
	return err
}

// DeviceEnroller connects device login approval to machine enrollment: the same enrollment the
// dashboard issues, minted for the workspace the account owner chose.
type DeviceEnroller struct{ Service *Service }

func (e DeviceEnroller) ValidateWorkspace(ctx context.Context, userID, workspaceID string) error {
	scope, err := workspace.Parse(workspaceID)
	if err == nil {
		err = workspace.Validate(ctx, e.Service.db, userID, scope)
	}
	if errors.Is(err, workspace.ErrInvalid) || errors.Is(err, workspace.ErrForbidden) {
		return auth.ErrWorkspaceForbidden
	}
	return err
}

// IssueEnrollment mints the enrollment for an approved grant; teamID is empty for Personal.
func (e DeviceEnroller) IssueEnrollment(ctx context.Context, userID, grantID, teamID, os string) (auth.DeviceEnrollment, error) {
	shell := "posix"
	if os == "windows" {
		shell = "powershell"
	}
	start, err := e.Service.IssueDeviceEnrollment(ctx, userID, grantID, teamID, shell)
	if err != nil {
		return auth.DeviceEnrollment{}, err
	}
	workspaceID := teamID
	if workspaceID == "" {
		workspaceID = "personal"
	}
	admission, err := e.Admission(ctx, userID, workspaceID)
	if err != nil {
		return auth.DeviceEnrollment{}, err
	}
	return auth.DeviceEnrollment{BootstrapToken: start.BootstrapToken, ServerURL: start.ServerURL, ExpiresAt: start.ExpiresAt, WorkspaceID: workspaceID, Admission: admission}, nil
}

func (e DeviceEnroller) Admission(ctx context.Context, userID, workspaceID string) (string, error) {
	if workspaceID == "personal" {
		return teams.AdmissionAccepted, nil
	}
	return teams.NewService(e.Service.db).AdmissionFor(ctx, userID, workspaceID)
}

func (e DeviceEnroller) EnrolledMachine(ctx context.Context, userID, machineID string) (auth.EnrolledMachine, bool, error) {
	alias, workspaces, ok, err := e.Service.EnrolledMachineWorkspaces(ctx, userID, machineID)
	if err != nil || !ok {
		return auth.EnrolledMachine{}, false, err
	}
	return auth.EnrolledMachine{ID: machineID, Alias: alias, Workspaces: workspaces}, true, nil
}
