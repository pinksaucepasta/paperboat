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
	self, peer := networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "host", Role: "machine"}}, networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "laptop", Role: "cli"}}
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
	self := networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "host", Role: "machine", WireGuardPublicKey: contractKey(0), DiscoPublicKey: contractKey(1), QUICCertificateFingerprint: strings.Repeat("ab", 32), QUICPublicKey: contractKey(2)}}
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
				peer := networkBinding{Binding: peernetwork.Binding{AccountID: account, EndpointID: fmt.Sprint("peer_", n), Role: "cli", WireGuardPublicKey: contractKey(n + 10), DiscoPublicKey: contractKey(n + 500)}}
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
	for i := range peernetwork.MaxEndpoints - len(peers) { // the complete endpoint bound
		peer := networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: fmt.Sprint("mesh_", i), Role: "cli", WireGuardPublicKey: contractKey(i + 1000), DiscoPublicKey: contractKey(i + 2000)}}
		peers = append(peers, networkPeer{Identity: peer, Scopes: []networkScope{personalNetworkScope(self, peer, 0)}})
	}
	// Mix scope-limited and peer-limited chunks. Thirty-two peer-only chunks are
	// insufficient even though this configuration stays below the shared total.
	for i := range 31 {
		s := peers[len(peers)-1-i].Scopes[0]
		for j := range 64 {
			s.ResourceID = fmt.Sprintf("scope_%d_%d", i, j)
			peers[len(peers)-1-i].Scopes = append(peers[len(peers)-1-i].Scopes, s)
		}
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
		chunks := relayGrantChunks(self, peers, peernetwork.MaxGrantPeers, expires.Unix())
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

// contractBinding is a complete registered identity (peernetwork.Binding.Valid) for the contract tests.
func contractBinding(account, endpoint, role string, n int) networkBinding {
	b := peernetwork.Binding{AccountID: account, EndpointID: endpoint, Role: role, EndpointGeneration: 1, KeyGeneration: 1, WireGuardPublicKey: contractKey(n), DiscoPublicKey: contractKey(n + 3000),
		QUICCertificateFingerprint: strings.Repeat("ab", 32), QUICPublicKey: contractKey(n + 6000), VirtualAddress: fmt.Sprintf("fd7a:115c:a1e0::%x", n+1)}
	if role == peernetwork.RoleMachine {
		b.MachineID, b.MachineGeneration = endpoint, 1
	}
	return networkBinding{Binding: b}
}

// The scopes the server emits for peer in self's configuration (network.go admissions, offline_access.go standing
// session rights, the account mesh pair), in self's direction.
func emittedConfigurationScopes(self, peer networkBinding, expiry int64) []networkScope {
	direction := "accept"
	if self.Role == peernetwork.RoleCLI {
		direction = "dial"
	}
	if self.Role == peer.Role {
		return []networkScope{personalNetworkScope(self, peer, 0)}
	}
	timed := func(kind, capability string) networkScope {
		return networkScope{ResourceKind: kind, ResourceID: "resource_" + kind + "_" + capability, ResourceGeneration: 1, Capability: capability, Direction: direction, Port: 443, ExpiresAt: expiry}
	}
	var out []networkScope
	for _, capability := range []string{"terminal", "exec", "managed_ssh", "private_access", "file_transfer"} {
		out = append(out, timed("machine_access", capability))
	}
	out = append(out, timed("inspector", "inspector"), timed("config_compare", "config_compare"), timed("recording", "recording"))
	host := self.EndpointID // offlineAccessPeers: the host's own resource
	if self.Role == peernetwork.RoleCLI {
		host = peer.EndpointID // teamHostPeers: the member's CLI names the host
	}
	for _, capability := range networkNames() {
		out = append(out, networkScope{ResourceKind: "machine_access", ResourceID: host, ResourceGeneration: 7, Capability: capability, Direction: direction, Port: 443})
	}
	return append(out, personalNetworkScope(self, peer, expiry))
}

// Every network configuration the server builds (configurationClaims), signed by the real mint, is one every CLI
// and daemon accepts: they decode it with peernetwork.DecodeSigned and check it with peernetwork.Configuration.Check,
// the rule this runs, and none of its scopes is one they would drop. Both roles of the recipient, every emitted scope.
func TestEveryConfigurationTheServerBuildsIsAcceptedByClients(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := mint.New([]mint.Key{{ID: "contract", PrivateKey: private}}, "contract", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{peernetwork.RoleMachine, peernetwork.RoleCLI} {
		self := contractBinding("account", "self_"+role, role, 1)
		var peers []networkPeer
		for i, peerRole := range []string{peernetwork.RoleMachine, peernetwork.RoleCLI} {
			if role == peernetwork.RoleCLI && peerRole == peernetwork.RoleCLI {
				continue // a CLI never peers with a CLI
			}
			peer := contractBinding("account", fmt.Sprint("peer_", i), peerRole, 10+i)
			peer.Endpoints = []string{"203.0.113.7:41641"}
			peers = append(peers, networkPeer{Identity: peer, Scopes: emittedConfigurationScopes(self, peer, now.Add(time.Minute).Unix())})
		}
		token, err := signer.SignNetworkConfiguration(configurationClaims("https://api.example.test", now, now.Add(time.Minute), 3, self, peers))
		if err != nil {
			t.Fatalf("%s: mint refused a configuration the server built: %v", role, err)
		}
		body, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
		if err != nil {
			t.Fatal(err)
		}
		var got peernetwork.Configuration
		if err := peernetwork.DecodeSigned(body, &got); err != nil {
			t.Fatalf("%s: clients refuse the signed claims: %v", role, err)
		}
		before := fmt.Sprint(got.Peers)
		if got.DropUnknownScopes(); fmt.Sprint(got.Peers) != before {
			t.Fatalf("%s: clients drop a scope the server emits", role)
		}
		if err := got.Check(now); err != nil || len(got.Peers) != len(peers) || len(got.Peers[0].Endpoints) != 1 {
			t.Fatalf("%s: clients refuse the configuration (%d of %d peers): %v", role, len(got.Peers), len(peers), err)
		}
	}
}

// The admitted scopes an endpoint reports are the kinds of the shared list.
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
	// A newer client's kind and a retried duplicate are skipped, never a refusal of the whole configuration
	// (audit-A P1-3); a malformed id still is.
	ids, err = admittedScopeIDs([]AdmittedScope{{ResourceKind: "unknown", ResourceID: "x"}, {ResourceKind: "recording", ResourceID: "r"}, {ResourceKind: "recording", ResourceID: "r"}})
	if err != nil || len(ids["unknown"]) != 0 || len(ids["recording"]) != 1 {
		t.Fatalf("ids %v, err %v", ids, err)
	}
	if _, err := admittedScopeIDs([]AdmittedScope{{ResourceKind: "recording", ResourceID: ""}}); err == nil {
		t.Fatal("an empty id was accepted")
	}
}

// Every regional candidate set the server builds (regionalCandidateSet), signed by the real mint, is one clients
// accept: DecodeSigned and peernetwork.RegionalCandidates.Check, with managed and self-hosted, pinned and unpinned
// relays, and the most nodes a set may name.
func TestEveryRegionalCandidateSetTheServerBuildsIsAcceptedByClients(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := mint.New([]mint.Key{{ID: "contract", PrivateKey: private}}, "contract", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var nodes []peernetwork.RegionalNode
	for i := range peernetwork.MaxRegionalCandidates {
		n := peernetwork.RegionalNode{NodeID: fmt.Sprint("node_", i), NodeGeneration: 1, ProcessEpoch: "epoch", Region: "region", FailureDomain: "domain", Roles: []string{"relay"}, Transports: []string{"derp_quic"},
			EndpointHost: "relay.example.test", EndpointTCPPort: 443, EndpointQUICPort: 443, State: "ready", ObservedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), CapacityLimit: 100, CapacityObservedAt: now.Unix()}
		if i%2 == 0 {
			n.Hosting, n.TLSSPKISHA256 = peernetwork.HostingSelfHosted, contractKey(i)
		}
		nodes = append(nodes, n)
	}
	token, err := signer.SignRegionalCandidates(regionalCandidateSet("https://api.example.test", "account", "endpoint", 3, now, now.Add(time.Hour), nodes))
	if err != nil {
		t.Fatalf("mint refused a candidate set the server built: %v", err)
	}
	body, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	var got peernetwork.RegionalCandidates
	if err := peernetwork.DecodeSigned(body, &got); err != nil || got.Check(now) != nil || len(got.Nodes) != len(nodes) {
		t.Fatalf("clients refuse the candidate set: %v, %v", err, got.Check(now))
	}
}
