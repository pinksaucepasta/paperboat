package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// pb ssh to a machine whose SSH the daemon reports as not ready names the reason and the recovery
// instead of OpenSSH's banner error (Lane W, F5: cloned sshd host keys).
func TestSSHToATargetThatIsNotReadyNamesTheReason(t *testing.T) {
	previous := readDaemonSnapshot
	t.Cleanup(func() { readDaemonSnapshot = previous })
	status := localapi.MachineStatus{ID: "machine_b", Alias: "box", SSHReadiness: "degraded", Health: []localapi.HealthItem{{Code: "ssh_host_keys_unpublished", Severity: "warning", Title: "The machine's SSH host keys are not published", Recovery: "On the machine, start an SSH server (sshd) if none runs"}}}
	readDaemonSnapshot = func(context.Context) (localapi.Snapshot, error) {
		return localapi.Snapshot{Machines: []localapi.MachineStatus{status}}, nil
	}
	err := sshTargetHealth(context.Background(), "machine_b", "box")
	message := commandFailureMessage(err, classifyCommandFailure(err))
	if err == nil || classifyCommandFailure(err).kind != commandRejected || !strings.Contains(message, "SSH to box is not ready: The machine's SSH host keys are not published. On the machine, start an SSH server") {
		t.Fatalf("err=%v message=%q", err, message)
	}
	status.SSHReadiness, status.Health = "ready", nil
	if err := sshTargetHealth(context.Background(), "machine_b", "box"); err != nil {
		t.Fatalf("a ready target was refused: %v", err)
	}
}
