package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// machineTeamHintBudget bounds the extra lookup on an already failed name resolution.
const machineTeamHintBudget = 5 * time.Second

// machineInTeamWorkspaceError is a machine name not found in the selected workspace that names a
// machine of one of the account's teams: it says which workspace to use instead of "no machine".
type machineInTeamWorkspaceError struct {
	machine, team string
}

func (e *machineInTeamWorkspaceError) Error() string {
	return e.machine + " is in team " + e.team + "; use --workspace " + e.team + "."
}
func (e *machineInTeamWorkspaceError) Unwrap() error { return resolver.ErrMachineNotFound }

// explainMachineInTeamWorkspace turns a machine-not-found from the selected workspace into
// machineInTeamWorkspaceError when exactly one other team workspace of the account has a machine with
// that name or ID (a team-owned machine lives in its team's workspace, not the personal one).
func explainMachineInTeamWorkspace(ctx context.Context, current *api.Client, scoped func(workspace string) (*api.Client, error), requested string, err error) error {
	if !errors.Is(err, resolver.ErrMachineNotFound) || current == nil || scoped == nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, machineTeamHintBudget)
	defer cancel()
	page, listErr := current.ListWorkspaces(ctx)
	if listErr != nil {
		return err
	}
	var holders []string
	for _, workspace := range page.Items {
		if workspace.Kind != "team" || workspace.ID == current.Workspace() {
			continue
		}
		client, scopeErr := scoped(workspace.ID)
		if scopeErr != nil {
			continue
		}
		machines, listErr := client.ListUserMachines(ctx)
		if listErr != nil {
			continue
		}
		for _, machine := range machines {
			if machine.ID == requested || strings.EqualFold(machine.Alias, requested) {
				holders = append(holders, workspace.ID)
				break
			}
		}
	}
	if len(holders) != 1 {
		return err
	}
	return &machineInTeamWorkspaceError{machine: requested, team: holders[0]}
}
