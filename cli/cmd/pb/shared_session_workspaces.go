package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// A session shared with you lives in its owner's workspace (often a team you are a member of), so
// `pb session shared` and `pb session join` look in every workspace the account can use unless the
// user chose one explicitly. Prior art: Tailscale puts machines shared in from another tailnet into
// the recipient's own netmap (tailcfg.Node.SharerUserID) instead of making them switch tailnets.

// sharedSessionWorkspaces is the client bound to each workspace a shared session may be in: only the
// selected one when the user chose it, otherwise every workspace of the account.
func sharedSessionWorkspaces(ctx context.Context, client *api.Client) ([]*api.Client, []api.Workspace, error) {
	if workspaceExplicit(ctx) {
		return []*api.Client{client}, []api.Workspace{{ID: client.Workspace()}}, nil
	}
	page, err := client.ListWorkspaces(ctx)
	if err != nil {
		return nil, nil, err
	}
	clients := make([]*api.Client, 0, len(page.Items))
	for _, workspace := range page.Items {
		scoped, err := client.ForWorkspace(workspace.ID)
		if err != nil {
			return nil, nil, err
		}
		clients = append(clients, scoped)
	}
	return clients, page.Items, nil
}

// sharedSessionsEverywhere is one bounded page of sessions shared with the account across its
// workspaces, and the servers' combined total.
func sharedSessionsEverywhere(ctx context.Context, client *api.Client, filters url.Values, maxItems int) ([]api.SharedTerminalSession, int, error) {
	clients, _, err := sharedSessionWorkspaces(ctx, client)
	if err != nil {
		return nil, 0, err
	}
	var all []api.SharedTerminalSession
	seen := map[string]bool{}
	total := 0
	for _, scoped := range clients {
		sessions, n, err := scoped.SharedTerminalSessionsPage(ctx, filters, maxItems)
		if err != nil {
			return nil, 0, err
		}
		total += n
		for _, s := range sessions {
			if seen[s.ID] {
				total--
				continue
			}
			seen[s.ID] = true
			all = append(all, s)
		}
	}
	if maxItems > 0 && len(all) > maxItems {
		all = all[:maxItems]
	}
	return all, total, nil
}

// sharedSessionInSeveralWorkspaces is a session ID that more than one workspace answers for.
type sharedSessionInSeveralWorkspacesError struct {
	id         string
	workspaces []string
}

func (e *sharedSessionInSeveralWorkspacesError) Error() string {
	return fmt.Sprintf("shared session %s is in several workspaces; choose one with %s", e.id, strings.Join(e.workspaces, " or "))
}

// sharedSessionWorkspace is the workspace that has session id. With no explicit choice it asks each of
// the account's workspaces; when none has it, the selected workspace is kept so the server's own
// "not found" reaches the user.
func sharedSessionWorkspace(ctx context.Context, client *api.Client, id string) (string, error) {
	if workspaceExplicit(ctx) {
		return client.Workspace(), nil
	}
	clients, workspaces, err := sharedSessionWorkspaces(ctx, client)
	if err != nil {
		return "", err
	}
	var found []string
	for i, scoped := range clients {
		_, err := scoped.TerminalSharing(ctx, id)
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusForbidden) {
			continue
		}
		if err != nil {
			return "", err
		}
		found = append(found, workspaces[i].ID)
	}
	switch len(found) {
	case 0:
		return client.Workspace(), nil
	case 1:
		return found[0], nil
	}
	ambiguous := &sharedSessionInSeveralWorkspacesError{id: id}
	for _, w := range found {
		ambiguous.workspaces = append(ambiguous.workspaces, "--workspace "+w)
	}
	return "", ambiguous
}
