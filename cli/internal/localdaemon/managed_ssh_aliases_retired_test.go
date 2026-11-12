package localdaemon

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestManagedSSHAliasTargetsSkipRetiredMachines(t *testing.T) {
	retired := []api.UserMachine{
		{ID: "machine_old", Alias: "hp-old", State: "disconnected", Online: true, InstallationGeneration: 1, SSH: &api.MachineSSH{OSUser: "u", Port: 22, TargetGeneration: 1}},
		{ID: "machine_gone", Alias: "hp-gone", State: "deleted", Online: true, InstallationGeneration: 1, SSH: &api.MachineSSH{OSUser: "u", Port: 22, TargetGeneration: 1}},
		{ID: "machine_revoked", Alias: "hp-revoked", State: "revoked", Online: true, InstallationGeneration: 1, SSH: &api.MachineSSH{OSUser: "u", Port: 22, TargetGeneration: 1}},
	}
	if got := managedSSHAliasTargets(retired); len(got) != 0 {
		t.Fatalf("retired machines became aliases: %+v", got)
	}
}

// X-41: a machine that uninstalled itself stays active but offline on the server; peers drop its alias and
// host keys, and get them back when it comes online again.
func TestManagedSSHAliasTargetsAndKnownHostsDropOfflineMachines(t *testing.T) {
	machine := api.UserMachine{ID: "machine_m", Alias: "macx-m", State: "active", InstallationGeneration: 1,
		SSH: &api.MachineSSH{OSUser: "u", Port: 22, TargetGeneration: 1}}
	if got := managedSSHAliasTargets([]api.UserMachine{machine}); len(got) != 0 {
		t.Fatalf("an offline machine kept its alias: %+v", got)
	}
	machine.Online = true
	if got := managedSSHAliasTargets([]api.UserMachine{machine}); len(got) != 1 {
		t.Fatalf("an online machine has no alias: %+v", got)
	}
}
