package peersessions

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// Every scope the server can put in a configuration must be one the client accepts, and every scope it puts in a
// relay grant one the relay accepts: either side refuses the whole document otherwise. Server 2026.10.23.121 emitted
// (machine_access, inspector) host rules the client did not know (every such host rejected its renewal until the
// rollback, 2026-10-27); server .7 put (recording, recording) in relay grants the relay did not know (every relayed
// pair of those accounts died, 2026-11-11). The relay's validator is peernetwork.ValidGrantScope (derpquic
// validScope), so this checks the exact rule relays apply to the grant scopes relayGrantScopes builds.
func TestEveryEmittedScopeIsAcceptedByTheClientAndTheRelay(t *testing.T) {
	const expiry, grantExpiry = 1_700_000_300, 1_700_000_600
	timed := func(kind, capability, direction string) networkScope {
		return networkScope{ResourceKind: kind, ResourceID: "resource_1", ResourceGeneration: 1, Capability: capability, Direction: direction, Port: 443, ExpiresAt: expiry}
	}
	var emitted []networkScope
	for _, direction := range []string{"dial", "accept"} {
		// Admission scopes (network.go grants): stored machine capabilities as projected, inspector, config compare,
		// a CLI recording replay; each ends at its admission's expiry.
		for _, capability := range []string{"terminal", "exec", "managed_ssh", "private_access", "file_transfer"} {
			emitted = append(emitted, timed("machine_access", capability, direction))
		}
		emitted = append(emitted, timed("inspector", "inspector", direction), timed("config_compare", "config_compare", direction), timed("recording", "recording", direction))
		// Standing team-machine session rights until revoked (offline_access.go), and the account mesh pair.
		for _, capability := range networkNames() {
			s := timed("machine_access", capability, direction)
			s.ResourceGeneration, s.ExpiresAt = 7, 0
			emitted = append(emitted, s)
		}
	}
	self, peer := networkBinding{AccountID: "account", EndpointID: "host", Role: "machine"}, networkBinding{AccountID: "account", EndpointID: "laptop", Role: "cli"}
	emitted = append(emitted, personalNetworkScope(self, peer, expiry), personalNetworkScope(self, peer, 0))

	for _, s := range emitted {
		// The client takes the mesh pair from its own account only (tailnet authority); every other scope from the list.
		if !peernetwork.ValidScope(s.ResourceKind, s.Capability) {
			t.Errorf("clients reject the emitted scope %s/%s", s.ResourceKind, s.Capability)
		}
	}
	for _, mesh := range []bool{false, true} {
		scopes := emitted
		if !mesh {
			scopes = scopes[:len(scopes)-2]
		}
		granted := relayGrantScopes(scopes, grantExpiry)
		if len(granted) == 0 {
			t.Fatal("no scopes reached the relay grant")
		}
		for _, s := range granted {
			if !peernetwork.ValidGrantScope(s) {
				t.Errorf("relays reject the granted scope %+v (mesh %v)", s, mesh)
			}
		}
	}
}

// The admitted scopes an endpoint reports are the kinds of the shared list; an unknown kind is refused.
func TestAdmittedScopesTakeEverySharedKind(t *testing.T) {
	var scopes []AdmittedScope
	peernetwork.Scopes(func(kind, capability string) bool {
		scopes = append(scopes, AdmittedScope{ResourceKind: kind, ResourceID: kind + "_" + capability})
		return true
	})
	ids, err := admittedScopeIDs(scopes)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scopes {
		if len(ids[s.ResourceKind]) == 0 {
			t.Errorf("%s admitted scopes dropped", s.ResourceKind)
		}
	}
	if _, err := admittedScopeIDs([]AdmittedScope{{ResourceKind: "unknown", ResourceID: "x"}}); err == nil {
		t.Fatal("an unknown kind was accepted")
	}
}
