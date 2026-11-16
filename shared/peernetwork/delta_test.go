package peernetwork

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func signTestConfiguration(t *testing.T, key ed25519.PrivateKey, c Configuration) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": ConfigurationType, "kid": "k1"})
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(unsigned)))
}

func meshTestConfiguration(now time.Time, generation uint64, peers ...int) Configuration {
	c := Configuration{Version: 1, Issuer: "https://api.example.test", Audience: ConfigurationAudience, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
		Generation: generation, Self: testBinding("machine_self", RoleMachine, 1), Peers: []Peer{}}
	for _, n := range peers {
		id := fmt.Sprintf("machine_%03d", n)
		scope := Scope{ResourceKind: "machine_network", ResourceID: "network_" + id, ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: NetworkPort}
		c.Peers = append(c.Peers, Peer{Identity: testBinding(id, RoleMachine, byte(10+n)), Scopes: []Scope{scope}})
	}
	return c
}

// A delta rebuilt on the held configuration is the server's exact signed token: one peer joins, one leaves, one
// changes its endpoints (the topic moves of a busy account), and an unchanged set carries no peer at all.
func TestConfigurationDeltaRebuildsTheSignedConfiguration(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_800_000_000, 0)
	held := meshTestConfiguration(now, 5, 1, 2, 3, 4)
	heldToken := signTestConfiguration(t, key, held)

	next := meshTestConfiguration(now.Add(time.Second), 6, 1, 3, 4, 5) // 2 left, 5 joined
	next.Peers[1].Endpoints = []string{"203.0.113.9:41641"}            // 3 moved
	nextToken := signTestConfiguration(t, key, next)
	d := NewConfigurationDelta(nextToken, next, 5, PeerDigests(held.Peers))
	if len(d.Configuration.Peers) != 2 || len(d.Removed) != 1 || d.Removed[0] != "machine_002" {
		t.Fatalf("delta carries %d peers and removes %v, want 2 and [machine_002]", len(d.Configuration.Peers), d.Removed)
	}
	wire, _ := json.Marshal(d)
	var received ConfigurationDelta
	if err := json.Unmarshal(wire, &received); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := received.Rebuild(heldToken)
	if err != nil || rebuilt != nextToken {
		t.Fatalf("rebuilt token differs from the signed one (err %v)", err)
	}
	parts := strings.Split(rebuilt, ".")
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatal("rebuilt token does not verify")
	}

	same := meshTestConfiguration(now.Add(2*time.Second), 7, 1, 3, 4, 5)
	same.Peers[1].Endpoints = next.Peers[1].Endpoints
	sameToken := signTestConfiguration(t, key, same)
	unchanged := NewConfigurationDelta(sameToken, same, 6, PeerDigests(next.Peers))
	if len(unchanged.Configuration.Peers) != 0 || len(unchanged.Removed) != 0 {
		t.Fatalf("unchanged set carries %d peers, removes %v", len(unchanged.Configuration.Peers), unchanged.Removed)
	}
	if got, err := unchanged.Rebuild(rebuilt); err != nil || got != sameToken {
		t.Fatalf("unchanged delta does not rebuild the signed token (err %v)", err)
	}

	// Everything removed: the rebuilt set is an empty list, as the server signs it.
	empty := meshTestConfiguration(now.Add(3*time.Second), 8)
	emptyToken := signTestConfiguration(t, key, empty)
	if got, err := NewConfigurationDelta(emptyToken, empty, 7, PeerDigests(same.Peers)).Rebuild(sameToken); err != nil || got != emptyToken {
		t.Fatalf("emptying delta does not rebuild the signed token (err %v)", err)
	}
}

// A delta for another base than the held configuration is refused (the client then asks for a full one).
func TestConfigurationDeltaRefusesAnotherBase(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_800_000_000, 0)
	held := meshTestConfiguration(now, 5, 1, 2)
	heldToken := signTestConfiguration(t, key, held)
	next := meshTestConfiguration(now, 7, 1)
	d := NewConfigurationDelta(signTestConfiguration(t, key, next), next, 6, PeerDigests(held.Peers))
	if _, err := d.Rebuild(heldToken); err == nil {
		t.Fatal("delta for generation 6 applied to generation 5")
	}
	d.BaseGeneration = 5
	d.Removed = []string{"machine_404"}
	if _, err := d.Rebuild(heldToken); err == nil {
		t.Fatal("delta removing a peer the held configuration lacks applied")
	}
	if _, err := d.Rebuild(""); err == nil || ConfigurationGeneration("") != 0 || ConfigurationGeneration(heldToken) != 5 {
		t.Fatal("no held configuration must mean base 0 and a refused delta")
	}
}
