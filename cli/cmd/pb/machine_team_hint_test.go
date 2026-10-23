package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// A team-owned machine lives in its team's workspace. Named from the personal workspace, pb says which
// workspace has it instead of "no machine has that name" (Lane O, pb exec laneo-pb, support_fa28d72d).
func TestAMachineInATeamWorkspaceIsNamedWithItsWorkspace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/workspaces":
			writeAPIData(t, w, map[string]any{"items": []map[string]string{
				{"id": "personal", "name": "Personal", "kind": "personal", "role": "owner"},
				{"id": "laneo-f13-trial", "name": "Trial", "kind": "team", "role": "owner"},
			}})
		case r.URL.Path == "/v1/machines" && r.URL.Query().Get("workspace") == "laneo-f13-trial":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "machine_pb", "alias": "laneo-pb"}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case r.URL.Path == "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []any{}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 0, "next_offset": nil}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	if err := client.SetWorkspace("personal"); err != nil {
		t.Fatal(err)
	}
	_, err := resolveUserMachine(context.Background(), client, "laneo-pb")
	message := commandFailureMessage(err, classifyCommandFailure(err))
	if !errors.Is(err, resolver.ErrMachineNotFound) || !strings.Contains(message, "laneo-pb is in team laneo-f13-trial; use --workspace laneo-f13-trial") {
		t.Fatalf("err=%v message=%q", err, message)
	}
	if _, err := resolveUserMachine(context.Background(), client, "nowhere"); err == nil || strings.Contains(err.Error(), "team") {
		t.Fatalf("an unknown machine named a team: %v", err)
	}
}
