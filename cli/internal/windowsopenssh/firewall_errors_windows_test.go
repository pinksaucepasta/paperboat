//go:build windows

package windowsopenssh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFirewallSnapshotFailureKeepsCauseWithoutTranscript(t *testing.T) {
	const privateTranscript = "private firewall program C:\\Users\\Alice\\OpenSSH\\sshd.exe"
	runnerCause := errors.New("PowerShell process failed")
	cause := fmt.Errorf("query diagnostic includes %s: %w", privateTranscript, runnerCause)
	root := t.TempDir()
	config := Config{
		Platform: "windows", InstallRoot: filepath.Join(root, "OpenSSH"), StateRoot: filepath.Join(root, "state"),
		ApprovedVersion: ApprovedVersion, ExpectedPublisher: "Microsoft", Port: 38222,
		Runner: verificationOutputRunner{output: []byte(privateTranscript), err: cause},
	}
	_, err := Provision(context.Background(), config)
	if !errors.Is(err, ErrFirewallSnapshot) || !errors.Is(err, cause) {
		t.Fatalf("snapshot error = %v, want firewall classification and runner cause", err)
	}
	if !errors.Is(err, runnerCause) {
		t.Fatalf("snapshot error lost nested runner cause: %v", err)
	}
	if strings.Contains(err.Error(), privateTranscript) {
		t.Fatalf("snapshot error exposed command output: %v", err)
	}
}

func TestFirewallDisableFailureKeepsOwnershipClassificationAndCause(t *testing.T) {
	const privateTranscript = "private firewall program C:\\Users\\Alice\\OpenSSH\\sshd.exe"
	runnerCause := errors.New("PowerShell process failed")
	cause := fmt.Errorf("mutation diagnostic includes %s: %w", privateTranscript, runnerCause)
	root := t.TempDir()
	config := Config{
		Platform: "windows", InstallRoot: filepath.Join(root, "OpenSSH"), StateRoot: filepath.Join(root, "state"),
		ApprovedVersion: ApprovedVersion, ExpectedPublisher: "Microsoft", Port: 38222,
	}
	rule := FirewallRule{
		Name: "OpenSSH-Server-In-TCP", DisplayName: "OpenSSH SSH Server", Direction: "Inbound",
		Action: "Allow", Enabled: true, Profiles: "Public", Program: `C:\Program Files\OpenSSH\sshd.exe`,
	}
	now := time.Now().UTC()
	state := FirewallState{
		Schema: firewallStateSchema,
		Before: FirewallSnapshot{CapturedAt: now},
		After:  FirewallSnapshot{CapturedAt: now, OpenSSHInbound: []FirewallRule{rule}},
		Owned:  []OwnedFirewallRule{{Rule: rule}},
	}
	stateBytes, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(config.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firewallStatePath(config), stateBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshotBytes, err := json.Marshal(FirewallSnapshot{CapturedAt: now, OpenSSHInbound: []FirewallRule{rule}})
	if err != nil {
		t.Fatal(err)
	}
	config.Runner = &queuedRunner{replies: []runnerReply{
		{output: snapshotBytes},
		{output: []byte(privateTranscript), err: cause},
	}}
	err = RepairFirewallOwnership(context.Background(), config)
	if !errors.Is(err, ErrRepairFailed) || !errors.Is(err, ErrFirewallOwnership) || errors.Is(err, ErrFirewallSnapshot) || !errors.Is(err, runnerCause) {
		t.Fatalf("ownership persistence error = %v, want ownership classification and runner cause", err)
	}
	if strings.Contains(err.Error(), privateTranscript) {
		t.Fatalf("ownership persistence error exposed command output: %v", err)
	}
}
