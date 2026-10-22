package localdaemon

import (
	"context"
	"sort"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

func managedSSHAliasTargets(ctx context.Context, client *api.Client) ([]managedssh.OpenSSHAliasTarget, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	machines, err := client.ListUserMachines(lookupCtx)
	if err != nil {
		return nil, managedSSHAuthorityError(err)
	}
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
	return targets, nil
}
