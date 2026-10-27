package peersessions

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// Every access scope the configuration can carry must be one the client accepts: server 2026.10.23.121
// emitted (machine_access, inspector) host rules the client did not know, and every such host rejected its
// whole renewal until the rollback (2026-10-27).
func TestEveryEmittedScopeIsOneTheClientAccepts(t *testing.T) {
	emitted := [][2]string{{"inspector", "inspector"}, {"config_compare", "config_compare"}}
	// network.go's grant projection of stored machine capabilities (terminal, exec, managed_ssh,
	// private_access as is; files as file_transfer).
	for _, capability := range []string{"terminal", "exec", "managed_ssh", "private_access", "file_transfer"} {
		emitted = append(emitted, [2]string{"machine_access", capability})
	}
	for _, capability := range networkNames() { // the host's standing rules (offline_access.go)
		emitted = append(emitted, [2]string{"machine_access", capability})
	}
	for _, scope := range emitted {
		if !peernetwork.ValidScope(scope[0], scope[1]) {
			t.Errorf("the server emits a %s scope with capability %q that clients reject", scope[0], scope[1])
		}
	}
}
