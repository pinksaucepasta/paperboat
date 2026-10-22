package localdaemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

// Alias targets come from the inventory itself: one list, never a lookup per machine.
func TestManagedSSHAliasTargetsReconcilesChangedAndRemovedMachines(t *testing.T) {
	var mu sync.RWMutex
	machines := []api.UserMachine{{
		ID: "machine_1", Alias: "victus", State: "active", InstallationGeneration: 1,
		SSH: &api.MachineSSH{OSUser: "Pujan", Port: 38222, TargetGeneration: 1},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/v1/machines" {
			t.Errorf("per-machine request %s %s; the inventory carries SSH targets", request.Method, request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		writeManagedSSHTestResponse(writer, api.UserMachinePage{Items: append([]api.UserMachine{}, machines...), Pagination: api.Pagination{Limit: 200, Total: len(machines)}})
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "test-token"}, server.Client())

	got, err := managedSSHAliasTargets(context.Background(), client)
	if err != nil {
		t.Fatalf("initial target reconciliation: %v", err)
	}
	assertManagedSSHAliasTarget(t, got, managedSSHExpectedAliasTarget{alias: "victus", user: "Pujan", port: 38222})

	mu.Lock()
	machines[0].InstallationGeneration = 2
	machines[0].SSH = &api.MachineSSH{OSUser: "Administrator", Port: 38223, TargetGeneration: 2}
	mu.Unlock()
	got, err = managedSSHAliasTargets(context.Background(), client)
	if err != nil {
		t.Fatalf("changed target reconciliation: %v", err)
	}
	assertManagedSSHAliasTarget(t, got, managedSSHExpectedAliasTarget{alias: "victus", user: "Administrator", port: 38223})

	mu.Lock()
	machines[0].InstallationGeneration = 3 // re-enrolled; its old target no longer applies
	mu.Unlock()
	if got, err = managedSSHAliasTargets(context.Background(), client); err != nil || len(got) != 0 {
		t.Fatalf("a target from an older installation became an alias: %+v %v", got, err)
	}

	mu.Lock()
	machines = nil
	mu.Unlock()
	got, err = managedSSHAliasTargets(context.Background(), client)
	if err != nil {
		t.Fatalf("removed target reconciliation: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("removed machine remained in aliases: %+v", got)
	}
}

type managedSSHExpectedAliasTarget struct {
	alias, user string
	port        uint16
}

func assertManagedSSHAliasTarget(t *testing.T, got []managedssh.OpenSSHAliasTarget, want managedSSHExpectedAliasTarget) {
	t.Helper()
	if len(got) != 1 || got[0].Alias != want.alias || got[0].User != want.user || got[0].Port != want.port {
		t.Fatalf("aliases=%+v, want one target %+v", got, want)
	}
}

func writeManagedSSHTestResponse(writer http.ResponseWriter, value any) {
	_ = json.NewEncoder(writer).Encode(map[string]any{"data": value})
}
