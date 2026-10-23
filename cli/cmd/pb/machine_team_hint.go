package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// machineOtherWorkspacesBudget bounds the lookup in the account's other workspaces.
const machineOtherWorkspacesBudget = 5 * time.Second

// machineResolutionNotice receives the one line naming the workspace a name resolved in.
var machineResolutionNotice io.Writer = os.Stderr

// machineInWorkspaces is a machine name not found in the selected workspace that matches machines in
// several other workspaces: the user picks one with --workspace.
type machineInWorkspacesError struct {
	machine    string
	candidates []string // "<machine-id> (workspace <id>)"
	workspaces []string
}

func (e *machineInWorkspacesError) Error() string {
	return fmt.Sprintf("%s matches machines in several workspaces: %s. Choose one with --workspace (%s).", e.machine, strings.Join(e.candidates, ", "), strings.Join(e.workspaces, ", "))
}
func (e *machineInWorkspacesError) Unwrap() error { return resolver.ErrMachineAmbiguous }

// resolveInOtherWorkspaces is the decision of 2026-10-23 (coordinator, for the user): a name not found
// in the selected workspace, when the user did not choose a workspace explicitly, is looked up in every
// other workspace the account can use. One match is used: client is rebound to its workspace and one
// line names it. Several matches are refused with the candidates. An explicit --workspace always wins.
// Prior art: Tailscale resolves names only in the current tailnet; kubectl and gh need an explicit
// context. This is friendlier, but never guesses between several.
func resolveInOtherWorkspaces(ctx context.Context, client *api.Client, requested string, notFound error) (api.UserMachine, error) {
	if !errors.Is(notFound, resolver.ErrMachineNotFound) || workspaceExplicit(ctx) {
		return api.UserMachine{}, notFound
	}
	lookup, cancel := context.WithTimeout(ctx, machineOtherWorkspacesBudget)
	defer cancel()
	page, err := client.ListWorkspaces(lookup)
	if err != nil {
		return api.UserMachine{}, notFound
	}
	type match struct {
		machine   api.UserMachine
		workspace api.Workspace
	}
	var matches []match
	for _, workspace := range page.Items {
		if workspace.ID == client.Workspace() {
			continue
		}
		scoped, err := client.ForWorkspace(workspace.ID)
		if err != nil {
			continue
		}
		machines, err := scoped.ListUserMachinesFiltered(lookup, url.Values{"q": {requested}})
		if err != nil {
			continue
		}
		if machine, err := resolveUserMachineIn(machines, requested); err == nil {
			matches = append(matches, match{machine, workspace})
		}
	}
	switch len(matches) {
	case 0:
		return api.UserMachine{}, notFound
	case 1:
		found := matches[0]
		if err := client.SetWorkspace(found.workspace.ID); err != nil {
			return api.UserMachine{}, err
		}
		_, _ = fmt.Fprintf(machineResolutionNotice, "%s (%s)\n", found.machine.Alias, describeWorkspace(found.workspace))
		return found.machine, nil
	}
	ambiguous := &machineInWorkspacesError{machine: requested}
	for _, found := range matches {
		ambiguous.candidates = append(ambiguous.candidates, found.machine.ID+" ("+describeWorkspace(found.workspace)+")")
		ambiguous.workspaces = append(ambiguous.workspaces, "--workspace "+found.workspace.ID)
	}
	return api.UserMachine{}, ambiguous
}

func describeWorkspace(workspace api.Workspace) string {
	if workspace.Kind == "team" {
		return "team " + workspace.ID
	}
	return workspace.ID + " workspace"
}
