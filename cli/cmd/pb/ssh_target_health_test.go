package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
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

// A machine with no SSH target (an OS user name managed SSH cannot serve) answers 404; pb ssh says why and what to
// do instead of "The requested Paperboat resource was not found" (X-28, OS user "Zoë Test").
func TestSSHToAMachineWithoutATargetNamesTheCauses(t *testing.T) {
	err := sshTargetLookupError("rx28-zoe", &api.APIError{Status: http.StatusNotFound, Code: "not_found"})
	message := commandFailureMessage(err, classifyCommandFailure(err))
	if classifyCommandFailure(err).kind != commandRejected || !strings.Contains(message, "SSH to rx28-zoe is not ready: Paperboat has no SSH target for it") ||
		!strings.Contains(message, "quote, comma") || !strings.Contains(message, "pb ssh doctor") || strings.Contains(message, "resource was not found") {
		t.Fatalf("message=%q", message)
	}
	other := &api.APIError{Status: http.StatusServiceUnavailable, Code: "managed_ssh_unavailable"}
	if got := sshTargetLookupError("rx28-zoe", other); got != error(other) {
		t.Fatalf("a non-404 error was rewritten: %v", got)
	}
}

// pb scp, sftp and rsync stop at the account's managed-SSH key limit with the daemon's reason instead of
// letting OpenSSH fail with "Permission denied (publickey)" (hp 2026-11-13, a full test account).
func TestSSHToolsNameTheClientKeyLimit(t *testing.T) {
	previous := readDaemonSnapshot
	t.Cleanup(func() { readDaemonSnapshot = previous })
	readDaemonSnapshot = func(context.Context) (localapi.Snapshot, error) {
		return localapi.Snapshot{Machines: []localapi.MachineStatus{{ID: "machine_b", Alias: "box", SSHReadiness: "degraded", Health: []localapi.HealthItem{{Code: "ssh_client_key_capacity", Severity: "warning", Title: "This computer cannot use managed SSH: the account's limit of 256 signed-in computers using managed SSH is reached", Recovery: "Sign out on a computer you no longer use (pb logout there) or revoke it (pb machine revoke <machine>), then retry"}}}}}, nil
	}
	err := sshTargetHealth(context.Background(), "machine_b", "box")
	if err == nil || !strings.Contains(err.Error(), "limit of 256") || !strings.Contains(err.Error(), "pb logout") {
		t.Fatalf("err=%v", err)
	}
}
