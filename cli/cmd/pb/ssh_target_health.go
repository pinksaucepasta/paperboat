package main

import (
	"context"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// sshTargetNotReadyError is a managed SSH connection to a machine whose SSH the daemon already knows is
// not ready: it names why and what to do instead of an OpenSSH banner error (Lane W, F5: cloned host
// keys showed as "kex_exchange_identification: Connection closed").
type sshTargetNotReadyError struct {
	machine, title, recovery string
}

func (e *sshTargetNotReadyError) Error() string {
	return "SSH to " + e.machine + " is not ready: " + strings.TrimSuffix(e.title, ".") + ". " + strings.TrimSuffix(e.recovery, ".") + "."
}

// sshTargetHealth returns sshTargetNotReadyError when the daemon's snapshot reports the machine's SSH as
// not ready with a health item, and nil otherwise (including when the daemon cannot be asked).
func sshTargetHealth(ctx context.Context, machineID, alias string) error {
	ctx, cancel := context.WithTimeout(ctx, relayDenialLookupTimeout)
	defer cancel()
	snapshot, err := readDaemonSnapshot(ctx)
	if err != nil {
		return nil
	}
	for _, machine := range snapshot.Machines {
		if machine.ID != machineID || machine.SSHReadiness == "ready" {
			continue
		}
		for _, item := range machine.Health {
			if sshHealthCode(item) {
				return &sshTargetNotReadyError{machine: alias, title: item.Title, recovery: item.Recovery}
			}
		}
	}
	return nil
}

// sshHealthCode reports the health items that explain an SSH that is not ready.
func sshHealthCode(item localapi.HealthItem) bool {
	return strings.HasPrefix(item.Code, "ssh_") && item.Title != ""
}
