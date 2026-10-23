package localdaemon

import (
	"sort"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

// managedSSHAliasTargets builds the OpenSSH aliases from the machines the inventory refresh just
// listed; it lists nothing itself (a second full listing per refresh was one of the largest loads).
func managedSSHAliasTargets(machines []api.UserMachine) []managedssh.OpenSSHAliasTarget {
	targets := make([]managedssh.OpenSSHAliasTarget, 0, len(machines))
	for _, machine := range machines {
		if machine.State == "revoked" || machine.State == "disconnected" || machine.State == "deleted" {
			continue
		}
		if ssh, ok := machine.CurrentSSH(); ok {
			targets = append(targets, managedssh.OpenSSHAliasTarget{Alias: machine.Alias, User: ssh.OSUser, Port: ssh.Port})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Alias < targets[j].Alias })
	return targets
}
