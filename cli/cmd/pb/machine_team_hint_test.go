package main

import (
	"bytes"
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

// workspaceMachinesServer serves the workspaces and, per workspace, the machines named in machines.
func workspaceMachinesServer(t *testing.T, machines map[string][]map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/workspaces":
			writeAPIData(t, w, map[string]any{"items": []map[string]string{
				{"id": "personal", "name": "Personal", "kind": "personal", "role": "owner"},
				{"id": "laneo-f13-trial", "name": "Trial", "kind": "team", "role": "owner"},
				{"id": "other-team", "name": "Other", "kind": "team", "role": "member"},
			}})
		case "/v1/machines":
			items := machines[r.URL.Query().Get("workspace")]
			if items == nil {
				items = []map[string]any{}
			}
			writeAPIData(t, w, map[string]any{"items": items, "pagination": map[string]any{"limit": 200, "offset": 0, "total": len(items), "next_offset": nil}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// Decision 2026-10-23 (coordinator, for the user): a name in exactly one other workspace is used and
// named; several are refused with the candidates; an explicit --workspace always wins.
func TestAMachineNameResolvesAcrossWorkspacesOnlyWhenUnique(t *testing.T) {
	var notice bytes.Buffer
	previous := machineResolutionNotice
	machineResolutionNotice = &notice
	t.Cleanup(func() { machineResolutionNotice = previous })
	personalClient := func(server *httptest.Server) *api.Client {
		client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
		if err := client.SetWorkspace("personal"); err != nil {
			t.Fatal(err)
		}
		return client
	}
	unique := workspaceMachinesServer(t, map[string][]map[string]any{"laneo-f13-trial": {{"id": "machine_pb", "alias": "laneo-pb"}}})
	client := personalClient(unique)
	machine, err := resolveUserMachine(context.Background(), client, "laneo-pb")
	if err != nil || machine.ID != "machine_pb" || client.Workspace() != "laneo-f13-trial" || notice.String() != "laneo-pb (team laneo-f13-trial)\n" {
		t.Fatalf("unique: machine=%+v err=%v workspace=%q notice=%q", machine, err, client.Workspace(), notice.String())
	}
	explicit := context.WithValue(context.Background(), workspaceExplicitKey{}, true)
	if _, err := resolveUserMachine(explicit, personalClient(unique), "laneo-pb"); !errors.Is(err, resolver.ErrMachineNotFound) {
		t.Fatalf("explicit --workspace looked elsewhere: %v", err)
	}
	several := workspaceMachinesServer(t, map[string][]map[string]any{
		"laneo-f13-trial": {{"id": "machine_pb", "alias": "laneo-pb"}},
		"other-team":      {{"id": "machine_other", "alias": "laneo-pb"}},
	})
	client = personalClient(several)
	_, err = resolveUserMachine(context.Background(), client, "laneo-pb")
	message := commandFailureMessage(err, classifyCommandFailure(err))
	if !errors.Is(err, resolver.ErrMachineAmbiguous) || client.Workspace() != "personal" || !strings.Contains(message, "--workspace laneo-f13-trial") || !strings.Contains(message, "--workspace other-team") || !strings.Contains(message, "machine_other (team other-team)") {
		t.Fatalf("ambiguous: err=%v workspace=%q message=%q", err, client.Workspace(), message)
	}
	if _, err := resolveUserMachine(context.Background(), personalClient(unique), "nowhere"); !errors.Is(err, resolver.ErrMachineNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

// Resolving one machine asks the server for that name only (q), not for the whole account
// (Lane X audit A2, F21-24).
func TestResolvingAMachineAsksOnlyForItsName(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("q"))
		items := []map[string]any{}
		if r.URL.Query().Get("q") == "build-host" {
			items = append(items, map[string]any{"id": "machine_1", "alias": "build-host"}, map[string]any{"id": "machine_2", "alias": "build-host-2"})
		}
		writeAPIData(t, w, map[string]any{"items": items, "pagination": map[string]any{"limit": 200, "offset": 0, "total": len(items), "next_offset": nil}})
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	machine, err := resolveUserMachine(context.Background(), client, "build-host")
	if err != nil || machine.ID != "machine_1" || len(queries) != 1 || queries[0] != "build-host" {
		t.Fatalf("machine=%+v err=%v queries=%v", machine, err, queries)
	}
}
