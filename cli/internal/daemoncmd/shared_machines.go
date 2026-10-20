package daemoncmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// privateAccessPermission is the team machine capability that lets a member reach the machine's
// private TCP services.
const privateAccessPermission = "private_access"

const (
	// A team lookup must not eat the inventory refresh's own time budget.
	sharedMachinesBudget       = 5 * time.Second
	maximumConcurrentTeamReads = 4
)

// teamSharedMachines remembers the last good answer: a slow or failed team lookup keeps the
// machines already offered instead of withdrawing them for a refresh and letting flows flap.
type teamSharedMachines struct {
	mu   sync.Mutex
	last []api.UserMachine
}

// join adds to the account's own machines every machine a team shared with this account together
// with private access, so a permitted member reaches it by name and by its 127.100.x.y address
// exactly like an own machine. A machine without that permission stays out of the topology: the
// server also refuses its flows, this keeps it from being offered at all. A failed lookup is
// returned next to the machines still known, so own machines never go offline because of it.
func (c *teamSharedMachines) join(ctx context.Context, own []api.UserMachine, scoped func(workspace string) (*api.Client, error)) ([]api.UserMachine, error) {
	ctx, cancel := context.WithTimeout(ctx, sharedMachinesBudget)
	defer cancel()
	shared, err := listTeamSharedMachines(ctx, scoped)
	c.mu.Lock()
	if err == nil {
		c.last = shared
	} else {
		shared = c.last
	}
	c.mu.Unlock()
	return mergeSharedMachines(own, shared), err
}

func mergeSharedMachines(own, shared []api.UserMachine) []api.UserMachine {
	known := make(map[string]bool, len(own)+len(shared))
	for _, machine := range own {
		known[machine.ID] = true
	}
	machines := slices.Clone(own)
	for _, machine := range shared {
		if known[machine.ID] || !slices.Contains(machine.Permissions, privateAccessPermission) {
			continue
		}
		known[machine.ID] = true
		machines = append(machines, machine)
	}
	return machines
}

func listTeamSharedMachines(ctx context.Context, scoped func(workspace string) (*api.Client, error)) ([]api.UserMachine, error) {
	root, err := scoped("")
	if err != nil {
		return nil, fmt.Errorf("list Paperboat workspaces: %w", err)
	}
	page, err := root.ListWorkspaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Paperboat workspaces: %w", err)
	}
	var teams []string
	for _, workspace := range page.Items {
		if workspace.Kind == "team" {
			teams = append(teams, workspace.ID)
		}
	}
	results := make([][]api.UserMachine, len(teams))
	failures := make([]error, len(teams))
	semaphore := make(chan struct{}, maximumConcurrentTeamReads)
	var wait sync.WaitGroup
	for index, team := range teams {
		wait.Add(1)
		go func() {
			defer wait.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			client, err := scoped(team)
			if err != nil {
				failures[index] = err
				return
			}
			machines, err := client.ListUserMachines(ctx)
			var unavailable *api.WorkspaceAccessError
			if errors.As(err, &unavailable) {
				return // the team was removed or suspended since the list above
			}
			if err != nil {
				failures[index] = fmt.Errorf("list machines shared by team %q: %w", team, err)
				return
			}
			results[index] = machines
		}()
	}
	wait.Wait()
	if err := errors.Join(failures...); err != nil {
		return nil, err
	}
	var shared []api.UserMachine
	for _, machines := range results {
		shared = append(shared, machines...)
	}
	return shared, nil
}
