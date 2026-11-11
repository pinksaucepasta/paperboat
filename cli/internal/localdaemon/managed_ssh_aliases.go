package localdaemon

import (
	"sort"
	"strings"

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
			targets = append(targets, managedssh.OpenSSHAliasTarget{Alias: machine.Alias, User: ssh.OSUser, Port: ssh.Port, AnyCaseUser: strings.EqualFold(machine.Platform, "windows")})
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

// withSharedSSHMachines adds machines teams shared for managed SSH to the account's own, so they get
// OpenSSH aliases and known_hosts lines too. An own machine wins an id or alias clash.
func withSharedSSHMachines(own, shared []api.UserMachine) []api.UserMachine {
	ids := make(map[string]bool, len(own)+len(shared))
	aliases := make(map[string]bool, len(own)+len(shared))
	for _, machine := range own {
		ids[machine.ID], aliases[machine.Alias] = true, true
	}
	machines := append([]api.UserMachine(nil), own...)
	for _, machine := range shared {
		if ids[machine.ID] || aliases[machine.Alias] {
			continue
		}
		ids[machine.ID], aliases[machine.Alias] = true, true
		machines = append(machines, machine)
	}
	return machines
}
