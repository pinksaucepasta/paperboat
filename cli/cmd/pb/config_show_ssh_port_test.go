package main

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

// `pb config set ssh-target-port` stores the port with the machine registration; config show
// used to hide it, so the setting looked like it had not been saved.
func TestConfigShowReportsTheSSHTargetPortThatWasSet(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", root)
	if port, ok := configuredSSHTargetPort(); ok || port != 0 {
		t.Fatalf("port %d reported before the machine has one", port)
	}
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	registration := identity.Registration{ServerURL: "https://api.example.test", MachineID: "machine_1", EnvironmentID: "machine_1", PublicKeyID: store.Current().ID, PublicIdentityKey: "key", InboxPath: root + "/inbox", InstallationGeneration: 1, SSHUser: "pb", SSHPort: 2222, UpdatedAt: time.Now().UTC()}
	if err := store.SaveRegistration(registration); err != nil {
		t.Fatal(err)
	}
	if port, ok := configuredSSHTargetPort(); !ok || port != 2222 {
		t.Fatalf("port=%d set=%v, want 2222", port, ok)
	}
}
