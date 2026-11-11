package peersessions

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/mint"
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

func contractKey(seed int) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{byte(seed%250 + 1), byte(seed / 250)}, 16))
}

// Every relay grant the server builds, signed by the real mint, is one relays and CLIs accept: they decode it with
// peernetwork.DecodeSigned (exact claims) and check it with peernetwork.Grant.Check, the rule this runs, and none of
// its scopes is one they would drop. It covers every shared scope, timed and standing, account mesh and team peers,
// peer relay on and off, and enough peers to need several chunks.
func TestEveryRelayGrantTheServerBuildsIsAcceptedByRelaysAndClients(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	expires := now.Add(time.Hour)
	self := networkBinding{AccountID: "account", EndpointID: "host", Role: "machine", WireGuardPublicKey: contractKey(0), DiscoPublicKey: contractKey(1), QUICCertificateFingerprint: strings.Repeat("ab", 32), QUICPublicKey: contractKey(2)}
	var peers []networkPeer
	n := 0
	peernetwork.Scopes(func(kind, capability string) bool {
		for _, standing := range []bool{false, true} {
			for _, team := range []bool{false, true} {
				n++
				account := "account"
				if team {
					account = "other"
				}
				peer := networkBinding{AccountID: account, EndpointID: fmt.Sprint("peer_", n), Role: "cli", WireGuardPublicKey: contractKey(n + 10), DiscoPublicKey: contractKey(n + 500)}
				s := networkScope{ResourceKind: kind, ResourceID: "resource_1", ResourceGeneration: 1, Capability: capability, Direction: "accept", Port: peernetwork.NetworkPort, ExpiresAt: now.Add(time.Minute).Unix()}
				if kind == "machine_network" {
					s = personalNetworkScope(self, peer, s.ExpiresAt)
				}
				if standing {
					s.ExpiresAt = 0
				}
				peers = append(peers, networkPeer{Identity: peer, Scopes: []networkScope{s}})
			}
		}
		return true
	})
	for i := range 3 * peernetwork.MaxGrantPeers { // several chunks
		peer := networkBinding{AccountID: "account", EndpointID: fmt.Sprint("mesh_", i), Role: "cli", WireGuardPublicKey: contractKey(i + 1000), DiscoPublicKey: contractKey(i + 2000)}
		peers = append(peers, networkPeer{Identity: peer, Scopes: []networkScope{personalNetworkScope(self, peer, 0)}})
	}
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := mint.New([]mint.Key{{ID: "contract", PrivateKey: private}}, "contract", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	base := peernetwork.RelayGrant{Version: 1, Issuer: "https://api.example.test", Audience: peernetwork.RelayGrantAudience, IssuedAt: now.Unix(), ExpiresAt: expires.Unix(), Generation: 9, AccountID: self.AccountID, EndpointID: self.EndpointID, WireGuardPublicKey: self.WireGuardPublicKey, DiscoPublicKey: self.DiscoPublicKey, CertificateFingerprint: self.QUICCertificateFingerprint, QUICPublicKey: self.QUICPublicKey, NodeID: "node", NodeGeneration: 1, ProcessEpoch: "epoch"}
	peerRelay := &peernetwork.RelayDescriptor{WireGuardPublicKey: contractKey(3), DiscoPublicKey: contractKey(4), VirtualAddress: "fd7a:115c:a1e0::9"}
	for _, relay := range []*peernetwork.RelayDescriptor{nil, peerRelay} {
		size := maximumRelayGrantPeers
		if relay != nil {
			size = maximumRelayControlPeers
		}
		chunks := relayGrantChunks(self, peers, size, expires.Unix())
		if len(chunks) < 2 || len(chunks) > peernetwork.MaxGrantParts {
			t.Fatalf("%d chunks", len(chunks))
		}
		named := 0
		for _, g := range relayGrantParts(base, chunks, relay) {
			token, err := signer.SignRelayGrant(g)
			if err != nil {
				t.Fatalf("mint refused a grant the server built: %v", err)
			}
			if len(token) > peernetwork.MaxGrantBytes {
				t.Fatalf("grant part of %d bytes", len(token))
			}
			body, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
			if err != nil {
				t.Fatal(err)
			}
			var got peernetwork.RelayGrant
			if err := peernetwork.DecodeSigned(body, &got); err != nil {
				t.Fatalf("receivers refuse the signed claims: %v", err)
			}
			before := fmt.Sprint(got.Peers, got.Memberships)
			if got.DropUnknownScopes(); fmt.Sprint(got.Peers, got.Memberships) != before {
				t.Fatal("receivers drop a scope the server emits")
			}
			if err := got.Check(now); err != nil {
				t.Fatalf("receivers refuse the grant: %v", err)
			}
			named += len(got.Peers)
		}
		relayable := 0
		for _, p := range peers {
			if len(relayGrantScopes(p.Scopes, expires.Unix())) > 0 {
				relayable++
			}
		}
		if named != relayable || relayable < len(peers)/2 {
			t.Fatalf("grants name %d of %d relayable peers (%d peers)", named, relayable, len(peers))
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
