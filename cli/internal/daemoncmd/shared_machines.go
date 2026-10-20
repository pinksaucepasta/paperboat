package daemoncmd

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// privateAccessPermission is the team machine capability that lets a member reach the machine's
// private TCP services.
const privateAccessPermission = "private_access"

// withTeamSharedMachines adds to the account's own machines every machine a team shared with this
// account together with private access, so a permitted member reaches it by name and by its
// 127.100.x.y address exactly like an own machine. A machine without that permission stays out of
// the topology: the server also refuses its flows, this keeps it from being offered at all.
func withTeamSharedMachines(ctx context.Context, own []api.UserMachine, scoped func(workspace string) (*api.Client, error)) ([]api.UserMachine, error) {
	root, err := scoped("")
	if err != nil {
		return nil, fmt.Errorf("list Paperboat workspaces: %w", err)
	}
	page, err := root.ListWorkspaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Paperboat workspaces: %w", err)
	}
	known := make(map[string]bool, len(own))
	for _, machine := range own {
		known[machine.ID] = true
	}
	machines := slices.Clone(own)
	for _, workspace := range page.Items {
		if workspace.Kind != "team" {
			continue
		}
		client, err := scoped(workspace.ID)
		if err != nil {
			return nil, err
		}
		shared, err := client.ListUserMachines(ctx)
		var unavailable *api.WorkspaceAccessError
		if errors.As(err, &unavailable) {
			continue // the team was removed or suspended since the list above
		}
		if err != nil {
			return nil, fmt.Errorf("list machines shared by team %q: %w", workspace.ID, err)
		}
		for _, machine := range shared {
			if known[machine.ID] || !slices.Contains(machine.Permissions, privateAccessPermission) {
				continue
			}
			known[machine.ID] = true
			machines = append(machines, machine)
		}
	}
	return machines, nil
}
