package localdaemon

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestManagedSSHAliasTargetsSkipRetiredMachines(t *testing.T) {
	retired := []api.UserMachine{
		{ID: "machine_old", Alias: "hp-old", State: "disconnected", InstallationGeneration: 1, SSH: &api.MachineSSH{OSUser: "u", Port: 22, TargetGeneration: 1}},
		{ID: "machine_gone", Alias: "hp-gone", State: "deleted", InstallationGeneration: 1, SSH: &api.MachineSSH{OSUser: "u", Port: 22, TargetGeneration: 1}},
		{ID: "machine_revoked", Alias: "hp-revoked", State: "revoked", InstallationGeneration: 1, SSH: &api.MachineSSH{OSUser: "u", Port: 22, TargetGeneration: 1}},
	}
	if got := managedSSHAliasTargets(retired); len(got) != 0 {
		t.Fatalf("retired machines became aliases: %+v", got)
	}
}
