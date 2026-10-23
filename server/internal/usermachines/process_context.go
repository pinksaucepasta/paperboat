package usermachines

import (
	"context"
	"database/sql"
	"errors"

	"github.com/pinksaucepasta/paperboat/server/internal/admission"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// terminalLaunchWorkspace reads creation ownership, never the current sharing
// audience or the workspace in which an existing terminal is being viewed.
func (s *Service) terminalLaunchWorkspace(ctx context.Context, sessionID, machineID string) (string, error) {
	var selected string
	err := s.db.SQL().QueryRowContext(ctx, `SELECT coalesce(owner_team_id,'personal') FROM user_machine_terminal_sessions WHERE id=$1 AND user_machine_id=$2`, sessionID, machineID).Scan(&selected)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrTerminalSessionNotFound
	}
	if err != nil {
		return "", err
	}
	if selected == "" {
		return "", ErrTerminalSessionNotFound
	}
	return selected, nil
}

func (s *Service) signProcessCredential(ctx context.Context, input mint.CredentialInput) (string, error) {
	switch input.CredentialClass {
	case "terminal_operation", "browser_terminal_operation":
		selected, err := s.terminalLaunchWorkspace(ctx, input.SessionID, input.MachineID)
		if err != nil {
			return "", err
		}
		if selected != "personal" && len(input.Scopes) == 1 && input.Scopes[0] == "terminal:operate" {
			if err := workspace.Validate(ctx, s.db, input.UserID, workspace.Scope{ID: selected, TeamID: selected}); err != nil {
				return "", err
			}
		}
		input.WorkspaceID = selected
	case "exec_operation", "ssh_operation":
		scope, selected := workspace.FromContext(ctx)
		if selected {
			if err := workspace.Validate(ctx, s.db, input.UserID, scope); err != nil {
				return "", err
			}
			input.WorkspaceID = "personal"
			if scope.TeamID != "" {
				input.WorkspaceID = scope.TeamID
			}
		} else {
			// Internal unscoped callers may only derive ownership from the
			// exact already-authorized target, never from request payloads.
			var owner, team string
			err := s.db.SQL().QueryRowContext(ctx, `SELECT user_id,coalesce(owner_team_id,'') FROM user_machines WHERE id=$1 AND revoked_at IS NULL AND deleted_at IS NULL`, input.MachineID).Scan(&owner, &team)
			if errors.Is(err, sql.ErrNoRows) {
				return "", ErrNotFound
			}
			if err != nil {
				return "", err
			}
			if owner != input.UserID {
				return "", ErrNotFound
			}
			input.WorkspaceID = "personal"
			if team != "" {
				input.WorkspaceID = team
			}
		}
	}
	input.ActorAccountID = input.UserID
	return admission.Sign(s.controlSigner, input)
}
