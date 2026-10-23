package localdaemon

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

// Alias targets come from the inventory's own list: no lookup per machine and no second listing.
func TestManagedSSHAliasTargetsReconcilesChangedAndRemovedMachines(t *testing.T) {
	machines := []api.UserMachine{{
		ID: "machine_1", Alias: "victus", State: "active", InstallationGeneration: 1,
		SSH: &api.MachineSSH{OSUser: "Pujan", Port: 38222, TargetGeneration: 1},
	}}
	assertManagedSSHAliasTarget(t, managedSSHAliasTargets(machines), managedSSHExpectedAliasTarget{alias: "victus", user: "Pujan", port: 38222})

	machines[0].InstallationGeneration = 2
	machines[0].SSH = &api.MachineSSH{OSUser: "Administrator", Port: 38223, TargetGeneration: 2}
	assertManagedSSHAliasTarget(t, managedSSHAliasTargets(machines), managedSSHExpectedAliasTarget{alias: "victus", user: "Administrator", port: 38223})

	machines[0].InstallationGeneration = 3 // re-enrolled; its old target no longer applies
	if got := managedSSHAliasTargets(machines); len(got) != 0 {
		t.Fatalf("a target from an older installation became an alias: %+v", got)
	}
	if got := managedSSHAliasTargets(nil); len(got) != 0 {
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
