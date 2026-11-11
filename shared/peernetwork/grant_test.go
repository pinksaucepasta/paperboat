package peernetwork

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func testKey(seed byte) string { return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32)) }

func validTestGrant(now time.Time) RelayGrant {
	scope := Scope{ResourceKind: "machine_network", ResourceID: "network_1", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: NetworkPort, ExpiresAt: now.Add(time.Minute).Unix()}
	return RelayGrant{Version: 1, Issuer: "https://api.example.test", Audience: RelayGrantAudience, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), Generation: 1,
		AccountID: "account", EndpointID: "endpoint", WireGuardPublicKey: testKey(1), DiscoPublicKey: testKey(2), CertificateFingerprint: strings.Repeat("ab", 32), QUICPublicKey: testKey(3),
		NodeID: "node", NodeGeneration: 1, ProcessEpoch: "epoch", Peers: []RelayPeer{{WireGuardPublicKey: testKey(4), DiscoPublicKey: testKey(5), Scopes: []Scope{scope}}},
		RelayControlPeers: []string{testKey(4)}, PeerRelay: &RelayDescriptor{WireGuardPublicKey: testKey(6), DiscoPublicKey: testKey(7), VirtualAddress: "fd7a:115c:a1e0::9"}}
}

func TestGrantCheckRefusesEachBrokenRule(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if err := validTestGrant(now).Check(now); err != nil {
		t.Fatalf("valid grant refused: %v", err)
	}
	membership := validTestGrant(now)
	membership.Memberships, membership.Peers, membership.RelayControlPeers, membership.PeerRelay = membership.Peers[0].Scopes, nil, nil, nil
	if err := membership.Check(now); err != nil {
		t.Fatalf("valid membership grant refused: %v", err)
	}
	for name, breakIt := range map[string]func(*RelayGrant){
		"version":                func(g *RelayGrant) { g.Version = 2 },
		"audience":               func(g *RelayGrant) { g.Audience = "paperboat-network" },
		"issuer":                 func(g *RelayGrant) { g.Issuer = "" },
		"future iat":             func(g *RelayGrant) { g.IssuedAt = now.Add(2 * MaxClockSkew).Unix() },
		"exp before iat":         func(g *RelayGrant) { g.ExpiresAt = g.IssuedAt },
		"lifetime":               func(g *RelayGrant) { g.ExpiresAt = g.IssuedAt + 13*3600 },
		"generation":             func(g *RelayGrant) { g.Generation = 0 },
		"account id":             func(g *RelayGrant) { g.AccountID = strings.Repeat("a", MaxGrantIDBytes+1) },
		"node":                   func(g *RelayGrant) { g.NodeID = "" },
		"node generation":        func(g *RelayGrant) { g.NodeGeneration = 0 },
		"epoch":                  func(g *RelayGrant) { g.ProcessEpoch = "" },
		"fingerprint":            func(g *RelayGrant) { g.CertificateFingerprint = strings.Repeat("AB", 32) },
		"quic key":               func(g *RelayGrant) { g.QUICPublicKey = "short" },
		"zero key":               func(g *RelayGrant) { g.WireGuardPublicKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32)) },
		"disco key":              func(g *RelayGrant) { g.DiscoPublicKey = "x" },
		"peer relay address":     func(g *RelayGrant) { g.PeerRelay.VirtualAddress = "fd00::1" },
		"peer relay no disco":    func(g *RelayGrant) { g.DiscoPublicKey = "" },
		"control self":           func(g *RelayGrant) { g.RelayControlPeers = []string{g.WireGuardPublicKey} },
		"control twice":          func(g *RelayGrant) { g.RelayControlPeers = []string{testKey(4), testKey(4)} },
		"peer twice":             func(g *RelayGrant) { g.Peers = append(g.Peers, g.Peers[0]) },
		"peer is self":           func(g *RelayGrant) { g.Peers[0].WireGuardPublicKey = g.WireGuardPublicKey },
		"peer without disco":     func(g *RelayGrant) { g.Peers[0].DiscoPublicKey = "" },
		"peer without scopes":    func(g *RelayGrant) { g.Peers[0].Scopes = nil },
		"scope port":             func(g *RelayGrant) { g.Peers[0].Scopes[0].Port = 22 },
		"scope without expiry":   func(g *RelayGrant) { g.Peers[0].Scopes[0].ExpiresAt = 0 },
		"unknown scope":          func(g *RelayGrant) { g.Peers[0].Scopes[0].Capability = "teleport" },
		"memberships and peers":  func(g *RelayGrant) { g.Memberships = g.Peers[0].Scopes },
		"too many peers":         func(g *RelayGrant) { g.Peers = make([]RelayPeer, MaxGrantPeers+1) },
		"too many control peers": func(g *RelayGrant) { g.RelayControlPeers = make([]string, MaxGrantPeers+1) },
		"too many scopes": func(g *RelayGrant) {
			for len(g.Peers[0].Scopes) <= MaxGrantScopes {
				s := g.Peers[0].Scopes[0]
				s.ResourceID = fmt.Sprint("network_", len(g.Peers[0].Scopes))
				g.Peers[0].Scopes = append(g.Peers[0].Scopes, s)
			}
		},
	} {
		g := validTestGrant(now)
		g.Peers[0].Scopes = append([]Scope(nil), g.Peers[0].Scopes...)
		breakIt(&g)
		if err := g.Check(now); !errors.Is(err, ErrInvalidDocument) {
			t.Errorf("%s: Check = %v, want ErrInvalidDocument", name, err)
		}
	}
	twice := membership
	twice.Memberships = append(twice.Memberships, twice.Memberships[0])
	many := membership
	many.Memberships = make([]Scope, MaxGrantMemberships+1)
	for i := range many.Memberships {
		many.Memberships[i] = membership.Memberships[0]
		many.Memberships[i].ResourceID = fmt.Sprint("network_", i)
	}
	for name, g := range map[string]RelayGrant{"duplicate membership": twice, "too many memberships": many} {
		if err := g.Check(now); !errors.Is(err, ErrInvalidDocument) {
			t.Errorf("%s: Check = %v, want ErrInvalidDocument", name, err)
		}
	}
}

// A scope of a kind or capability this build does not know (a newer server's) authorizes nothing here; it never
// fails the grant, and a peer left with no scope is left out. A malformed known scope still fails.
func TestReceiversDropUnknownScopesInsteadOfFailingTheGrant(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := validTestGrant(now)
	known := g.Peers[0].Scopes[0]
	unknownKind, unknownCapability := known, known
	unknownKind.ResourceKind, unknownCapability.Capability = "future_kind", "future_capability"
	g.Peers[0].Scopes = []Scope{known, unknownKind}
	g.Peers = append(g.Peers, RelayPeer{WireGuardPublicKey: testKey(8), DiscoPublicKey: testKey(9), Scopes: []Scope{unknownCapability}})
	g.DropUnknownScopes()
	if len(g.Peers) != 1 || len(g.Peers[0].Scopes) != 1 || g.Peers[0].Scopes[0] != known {
		t.Fatalf("peers after dropping unknown scopes: %+v", g.Peers)
	}
	if err := g.Check(now); err != nil {
		t.Fatalf("grant with unknown scopes dropped: %v", err)
	}
	m := validTestGrant(now)
	m.Peers, m.RelayControlPeers, m.PeerRelay = nil, nil, nil
	m.Memberships = []Scope{known, unknownKind}
	m.DropUnknownScopes()
	if len(m.Memberships) != 1 || m.Check(now) != nil {
		t.Fatalf("memberships after dropping unknown scopes: %+v", m.Memberships)
	}
}

// Signed claims stay exact: an unknown claim could be a restriction this build does not understand.
func TestDecodeSignedRefusesUnknownClaimsAndTrailingData(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	raw, err := json.Marshal(validTestGrant(now))
	if err != nil {
		t.Fatal(err)
	}
	var g RelayGrant
	if err := DecodeSigned(raw, &g); err != nil || g.Check(now) != nil {
		t.Fatalf("round trip: %v", err)
	}
	extra := append(bytes.TrimSuffix(raw, []byte("}")), []byte(`,"restriction":true}`)...)
	for name, body := range map[string][]byte{"unknown claim": extra, "trailing": append(raw, []byte(`{}`)...)} {
		if err := DecodeSigned(body, new(RelayGrant)); !errors.Is(err, ErrInvalidDocument) {
			t.Errorf("%s: DecodeSigned = %v", name, err)
		}
	}
}
