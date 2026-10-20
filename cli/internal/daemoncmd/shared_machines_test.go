package daemoncmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

func sharedMachinesServer(t *testing.T, byWorkspace map[string][]api.UserMachine) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/workspaces":
			_, _ = w.Write([]byte(`{"data":{"items":[{"id":"personal","name":"Personal","kind":"personal","role":"owner"},{"id":"team-a","name":"A","kind":"team","role":"member"},{"id":"team-b","name":"B","kind":"team","role":"member"}]}}`))
		case "/v1/machines":
			machines, ok := byWorkspace[r.URL.Query().Get("workspace")]
			if !ok {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":"workspace_unavailable","message":"not available"}}`))
				return
			}
			body, _ := json.Marshal(map[string]any{"data": map[string]any{"items": machines, "pagination": map[string]any{"limit": 200, "offset": 0, "total": len(machines), "next_offset": nil}}})
			_, _ = w.Write(body)
		default:
			t.Errorf("unexpected request %s", r.URL.RequestURI())
		}
	}))
}

func TestTeamSharedMachinesJoinTopologyOnlyWithPrivateAccess(t *testing.T) {
	own := api.UserMachine{ID: "machine_own", Alias: "own", Permissions: []string{"terminal"}}
	shared := api.UserMachine{ID: "machine_shared", Alias: "shared", AssignedIP: "127.100.0.9", Permissions: []string{"terminal", "private_access"}}
	terminalOnly := api.UserMachine{ID: "machine_terminal", Alias: "terminal-only", Permissions: []string{"terminal"}}
	server := sharedMachinesServer(t, map[string][]api.UserMachine{
		"team-a": {shared, terminalOnly, own},
		"team-b": {shared},
	})
	defer server.Close()
	scoped := func(workspace string) (*api.Client, error) {
		client := api.New(server.URL, config.Credential{AccessToken: "session"}, server.Client())
		return client, client.SetWorkspace(workspace)
	}
	machines, err := withTeamSharedMachines(context.Background(), []api.UserMachine{own}, scoped)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, machine := range machines {
		ids = append(ids, machine.ID)
	}
	if len(ids) != 2 || ids[0] != "machine_own" || ids[1] != "machine_shared" {
		t.Fatalf("topology machines = %v, want the own machine and the one machine shared with private access, once", ids)
	}
}

func TestTeamSharedMachinesSkipAWorkspaceThatIsNoLongerAvailable(t *testing.T) {
	shared := api.UserMachine{ID: "machine_shared", Alias: "shared", Permissions: []string{"private_access"}}
	server := sharedMachinesServer(t, map[string][]api.UserMachine{"team-b": {shared}})
	defer server.Close()
	scoped := func(workspace string) (*api.Client, error) {
		client := api.New(server.URL, config.Credential{AccessToken: "session"}, server.Client())
		return client, client.SetWorkspace(workspace)
	}
	machines, err := withTeamSharedMachines(context.Background(), nil, scoped)
	if err != nil || len(machines) != 1 || machines[0].ID != "machine_shared" {
		t.Fatalf("machines = %v, err = %v; the team that disappeared must not hide the other team's machine", machines, err)
	}
}
