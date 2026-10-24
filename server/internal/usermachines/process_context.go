package usermachines

import (
	"context"
	"database/sql"
	"errors"

	"github.com/pinksaucepasta/paperboat/server/internal/admission"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// terminalLaunchWorkspaces reads creation ownership (personal or the creating team) of each session on its
// machine, never the current sharing audience or the workspace in which an existing terminal is being viewed.
// Sessions not found on their machine are absent.
func (s *Service) terminalLaunchWorkspaces(ctx context.Context, sessions, machines []string) (map[string]string, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT s.id,coalesce(s.owner_team_id,'personal') FROM unnest($1::text[],$2::text[]) AS w(id,machine)
 JOIN user_machine_terminal_sessions s ON s.id=w.id AND s.user_machine_id=w.machine`, sessions, machines)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for rows.Next() {
		var id, selected string
		if err := rows.Scan(&id, &selected); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if selected != "" {
			out[id] = selected
		}
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// signTerminalCredential signs a terminal operation for the session's launch workspace: an operator credential
// for a team terminal needs the owner to still be in that team.
func (s *Service) signTerminalCredential(ctx context.Context, input mint.CredentialInput, selected string, validate func(user, team string) error) (string, error) {
	if selected != "personal" && len(input.Scopes) == 1 && input.Scopes[0] == "terminal:operate" {
		if err := validate(input.UserID, selected); err != nil {
			return "", err
		}
	}
	input.WorkspaceID = selected
	input.ActorAccountID = input.UserID
	return admission.Sign(s.controlSigner, input)
}

func (s *Service) validateWorkspace(ctx context.Context) func(user, team string) error {
	return func(user, team string) error {
		return workspace.Validate(ctx, s.db, user, workspace.Scope{ID: team, TeamID: team})
	}
}

func (s *Service) signProcessCredential(ctx context.Context, input mint.CredentialInput) (string, error) {
	switch input.CredentialClass {
	case "terminal_operation", "browser_terminal_operation":
		launch, err := s.terminalLaunchWorkspaces(ctx, []string{input.SessionID}, []string{input.MachineID})
		if err != nil {
			return "", err
		}
		selected, found := launch[input.SessionID]
		if !found {
			return "", ErrTerminalSessionNotFound
		}
		return s.signTerminalCredential(ctx, input, selected, s.validateWorkspace(ctx))
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
