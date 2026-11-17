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

func testKey(seed byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32))
}

func validTestGrant(now time.Time) RelayGrant {
	scope := Scope{ResourceKind: "machine_network", ResourceID: "network_1", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: NetworkPort, ExpiresAt: now.Add(time.Minute).Unix()}
	return RelayGrant{Version: 1, Issuer: "https://api.example.test", Audience: RelayGrantAudience, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), Generation: 1,
		AccountID: "account", EndpointID: "endpoint", WireGuardPublicKey: testKey(1), DiscoPublicKey: testKey(2), CertificateFingerprint: strings.Repeat("ab", 32), QUICPublicKey: testKey(3),
		NodeID: "node", NodeGeneration: 1, ProcessEpoch: "epoch", Memberships: []Scope{scope},
		PeerRelay: &RelayDescriptor{WireGuardPublicKey: testKey(6), DiscoPublicKey: testKey(7), VirtualAddress: "fd7a:115c:a1e0::9"}}
}

func TestGrantCheckRefusesEachBrokenRule(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if err := validTestGrant(now).Check(now); err != nil {
		t.Fatalf("valid grant refused: %v", err)
	}
	for name, breakIt := range map[string]func(*RelayGrant){
		"version":              func(g *RelayGrant) { g.Version = 2 },
		"audience":             func(g *RelayGrant) { g.Audience = "paperboat-network" },
		"issuer":               func(g *RelayGrant) { g.Issuer = "" },
		"future iat":           func(g *RelayGrant) { g.IssuedAt = now.Add(2 * MaxClockSkew).Unix() },
		"exp before iat":       func(g *RelayGrant) { g.ExpiresAt = g.IssuedAt },
		"lifetime":             func(g *RelayGrant) { g.ExpiresAt = g.IssuedAt + 13*3600 },
		"generation":           func(g *RelayGrant) { g.Generation = 0 },
		"account id":           func(g *RelayGrant) { g.AccountID = strings.Repeat("a", MaxGrantIDBytes+1) },
		"node":                 func(g *RelayGrant) { g.NodeID = "" },
		"node generation":      func(g *RelayGrant) { g.NodeGeneration = 0 },
		"epoch":                func(g *RelayGrant) { g.ProcessEpoch = "" },
		"fingerprint":          func(g *RelayGrant) { g.CertificateFingerprint = strings.Repeat("AB", 32) },
		"quic key":             func(g *RelayGrant) { g.QUICPublicKey = "short" },
		"zero key":             func(g *RelayGrant) { g.WireGuardPublicKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32)) },
		"disco key":            func(g *RelayGrant) { g.DiscoPublicKey = "x" },
		"peer relay address":   func(g *RelayGrant) { g.PeerRelay.VirtualAddress = "fd00::1" },
		"peer relay no disco":  func(g *RelayGrant) { g.DiscoPublicKey = "" },
		"scope port":           func(g *RelayGrant) { g.Memberships[0].Port = 22 },
		"scope without expiry": func(g *RelayGrant) { g.Memberships[0].ExpiresAt = 0 },
		"unknown scope":        func(g *RelayGrant) { g.Memberships[0].Capability = "teleport" },
		"membership twice":     func(g *RelayGrant) { g.Memberships = append(g.Memberships, g.Memberships[0]) },
		"too many memberships": func(g *RelayGrant) {
			for len(g.Memberships) <= MaxGrantMemberships {
				s := g.Memberships[0]
				s.ResourceID = fmt.Sprint("network_", len(g.Memberships))
				g.Memberships = append(g.Memberships, s)
			}
		},
	} {
		g := validTestGrant(now)
		g.Memberships = append([]Scope(nil), g.Memberships...)
		breakIt(&g)
		if err := g.Check(now); !errors.Is(err, ErrInvalidDocument) {
			t.Errorf("%s: Check = %v, want ErrInvalidDocument", name, err)
		}
	}
}

// A membership of a kind or capability this build does not know (a newer server's) authorizes nothing here; it never
// fails the grant. A malformed known scope still fails.
func TestReceiversDropUnknownScopesInsteadOfFailingTheGrant(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := validTestGrant(now)
	known := g.Memberships[0]
	unknownKind, unknownCapability := known, known
	unknownKind.ResourceKind, unknownCapability.Capability = "future_kind", "future_capability"
	g.Memberships = []Scope{known, unknownKind, unknownCapability}
	g.DropUnknownScopes()
	if len(g.Memberships) != 1 || g.Memberships[0] != known || g.Check(now) != nil {
		t.Fatalf("memberships after dropping unknown scopes: %+v", g.Memberships)
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
	// The retired per-peer list is an unknown claim now, empty or not.
	empty := append(bytes.TrimSuffix(raw, []byte("}")), []byte(`,"peers":[]}`)...)
	for name, body := range map[string][]byte{"unknown claim": extra, "retired peers list": empty, "trailing": append(raw, []byte(`{}`)...)} {
		if err := DecodeSigned(body, new(RelayGrant)); !errors.Is(err, ErrInvalidDocument) {
			t.Errorf("%s: DecodeSigned = %v", name, err)
		}
	}
}
