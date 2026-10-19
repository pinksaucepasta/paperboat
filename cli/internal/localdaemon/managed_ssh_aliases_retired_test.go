package localdaemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestManagedSSHAliasTargetsNeverAskForRetiredMachines(t *testing.T) {
	var lookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/v1/machines":
			writeManagedSSHTestResponse(writer, api.UserMachinePage{Items: []api.UserMachine{
				{ID: "machine_old", Alias: "hp-old", State: "disconnected", InstallationGeneration: 1},
				{ID: "machine_gone", Alias: "hp-gone", State: "deleted", InstallationGeneration: 1},
			}, Pagination: api.Pagination{Limit: 200, Total: 2}})
		case strings.HasSuffix(request.URL.Path, "/ssh-target"):
			lookups.Add(1)
			http.NotFound(writer, request)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "test-token"}, server.Client())
	if _, err := managedSSHAliasTargets(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if got := lookups.Load(); got != 0 {
		t.Fatalf("%d ssh-target lookups for disconnected or deleted machines; every refresh would hit the server and get 404", got)
	}
}
