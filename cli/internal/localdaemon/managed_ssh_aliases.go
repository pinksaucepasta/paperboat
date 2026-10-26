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

// managedSSHKnownHosts is the known_hosts content for every listed machine's active host keys, as the inventory
// carries them, so OpenSSH verifies a managed host from a local file instead of running a helper per
// connection (F5-07). A machine whose keys do not parse is left out: OpenSSH then refuses it as unknown.
func managedSSHKnownHosts(machines []api.UserMachine, domain string) []byte {
	var content []byte
	for _, machine := range machines {
		ssh, ok := machine.CurrentSSH()
		if !ok || machine.State == "revoked" || machine.State == "disconnected" || machine.State == "deleted" {
			continue
		}
		keys, err := managedssh.ParseHostPublicKeys(ssh.HostKeys)
		if err != nil {
			continue
		}
		if lines, err := managedssh.FormatKnownHosts(machine.Alias+"."+domain, ssh.Port, keys); err == nil {
			content = append(content, lines...)
		}
	}
	return content
}
