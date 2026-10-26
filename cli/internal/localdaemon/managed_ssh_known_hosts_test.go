package localdaemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// F5-07: OpenSSH verifies managed hosts from the daemon's known_hosts file, written from the inventory's host keys.
func TestManagedSSHKnownHostsListsEachCurrentMachine(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	machine := func(alias, state string, port uint16) api.UserMachine {
		return api.UserMachine{ID: "machine_" + alias, Alias: alias, State: state, Online: true, InstallationGeneration: 2, SSH: &api.MachineSSH{OSUser: "pbt", Port: port, TargetGeneration: 2, HostKeyGeneration: 2, HostKeys: []string{line}}}
	}
	got := string(managedSSHKnownHosts([]api.UserMachine{machine("box", "ready", 22), machine("alt", "ready", 2222), machine("gone", "revoked", 22)}, "example.test"))
	for _, want := range []string{"box.example.test " + line, "[alt.example.test]:2222 " + line} {
		if !strings.Contains(got, want) {
			t.Fatalf("known_hosts lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "gone.example.test") {
		t.Fatalf("a revoked machine is trusted:\n%s", got)
	}
}
