package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// F5-08 / F21-11: the last applied capability policy holds across a restart while Paperboat is
// unreachable; another installation generation's file is ignored; the first server answer wins,
// including one that turns a capability off.
func TestMachineCapabilitiesPersistAcrossRestartForThisInstallationOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine-capabilities.json")
	first := newMachineCapabilityController(nil)
	first.Persist(path, 4)
	if first.Enabled("ssh.v1") {
		t.Fatal("a fresh installation opened managed SSH before any policy")
	}
	on := machineCapabilityPolicy{Schema: machineCapabilitiesSchemaV1, DesiredVersion: 7, Desired: machineCapabilitySelection{Terminal: true, ManagedSSH: true}}
	if err := first.Apply(context.Background(), on); err != nil {
		t.Fatal(err)
	}

	restarted := newMachineCapabilityController(nil)
	restarted.Persist(path, 4)
	if !restarted.Enabled("ssh.v1") || !restarted.Enabled("terminal.v1") || restarted.Enabled("file-transfer.v1") {
		t.Fatal("a restart did not keep the last applied policy")
	}
	if restarted.Observation(time.Now()) != nil {
		t.Fatal("a loaded policy was reported as applied before the server answered")
	}

	otherInstallation := newMachineCapabilityController(nil)
	otherInstallation.Persist(path, 5)
	if otherInstallation.Enabled("ssh.v1") {
		t.Fatal("a policy saved by another installation generation was used")
	}

	off := machineCapabilityPolicy{Schema: machineCapabilitiesSchemaV1, DesiredVersion: 7, Desired: machineCapabilitySelection{Terminal: true}}
	if err := restarted.Apply(context.Background(), off); err != nil {
		t.Fatal(err)
	}
	if restarted.Enabled("ssh.v1") {
		t.Fatal("the server turning managed SSH off did not win over the saved policy")
	}
	again := newMachineCapabilityController(nil)
	again.Persist(path, 4)
	if again.Enabled("ssh.v1") {
		t.Fatal("the saved policy was not updated by the server's answer")
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := newMachineCapabilityController(nil)
	broken.Persist(path, 4)
	if broken.Enabled("terminal.v1") {
		t.Fatal("an unreadable saved policy opened a gate")
	}
}
