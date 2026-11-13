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

// An asleep machine keeps its alias and host keys (Tailscale keeps offline peers' names); only retired
// machines drop, and a machine that uninstalled itself is retired by pb uninstall telling the server (X-41).
func TestManagedSSHAliasTargetsKeepOfflineMachines(t *testing.T) {
	machine := api.UserMachine{ID: "machine_m", Alias: "macx-m", State: "active", Online: false, InstallationGeneration: 1,
		SSH: &api.MachineSSH{OSUser: "u", Port: 22, TargetGeneration: 1}}
	if got := managedSSHAliasTargets([]api.UserMachine{machine}); len(got) != 1 {
		t.Fatalf("an offline machine lost its alias: %+v", got)
	}
}
