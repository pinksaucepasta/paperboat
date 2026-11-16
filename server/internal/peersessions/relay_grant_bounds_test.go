package peersessions

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// The largest grant the server builds, signed by mint, fits the relay's token bound: the membership bound is the
// relay's own, and every membership carries a resource id of the longest length a scope may have.
func TestTheLargestRelayGrantFitsTheRelayTokenBound(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	provider, err := mint.New([]mint.Key{{ID: "k", PrivateKey: ed25519.NewKeyFromSeed(seed)}}, "k", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	key := func(i int) string { return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%032d", i)) }
	self := networkBinding{Binding: peernetwork.Binding{AccountID: strings.Repeat("a", 256), EndpointID: strings.Repeat("e", 256), WireGuardPublicKey: key(0)}}
	now := time.Now().Unix()
	var peers []networkPeer
	for i := 1; i <= 999; i++ { // other-account peers, each with a distinct maximal session scope
		id := fmt.Sprintf("%0*d", peernetwork.MaxResourceIDBytes, i)
		peers = append(peers, networkPeer{Identity: networkBinding{Binding: peernetwork.Binding{AccountID: "other", EndpointID: fmt.Sprint(i), WireGuardPublicKey: key(i), DiscoPublicKey: key(i)}},
			Scopes: []networkScope{{ResourceKind: "machine_access", ResourceID: id, ResourceGeneration: 1 << 53, Capability: "file_transfer", Direction: "accept", Port: peernetwork.NetworkPort, ExpiresAt: now + 60}}})
	}
	memberships := relayGrantMemberships(self, peers, now+60)
	parts := relayGrantMembershipParts(memberships)
	if len(memberships) != 999 || len(parts) < 2 || len(parts) >= peernetwork.MaxGrantParts {
		t.Fatalf("%d memberships in %d parts, want all 999 within %d parts", len(memberships), len(parts), peernetwork.MaxGrantParts)
	}
	kept := 0
	for _, part := range parts {
		grant := peernetwork.RelayGrant{Version: 1, Issuer: strings.Repeat("i", 256), Audience: peernetwork.RelayGrantAudience, IssuedAt: now, ExpiresAt: now + 60, Generation: 1 << 53,
			AccountID: self.AccountID, EndpointID: self.EndpointID, WireGuardPublicKey: self.WireGuardPublicKey, DiscoPublicKey: key(0), CertificateFingerprint: strings.Repeat("f", 64),
			QUICPublicKey: key(0), NodeID: strings.Repeat("n", 256), NodeGeneration: 1 << 53, ProcessEpoch: strings.Repeat("p", 256), Peers: []peernetwork.RelayPeer{}, Memberships: part}
		token, err := provider.SignRelayGrant(grant)
		if err != nil {
			t.Fatal(err)
		}
		if len(token) > peernetwork.MaxGrantBytes {
			t.Fatalf("a part of %d memberships signs to %d bytes, over the relay's %d", len(part), len(token), peernetwork.MaxGrantBytes)
		}
		kept += len(part)
	}
	if kept != 999 {
		t.Fatalf("parts carry %d memberships, want 999", kept)
	}
	t.Logf("999 maximal memberships in %d parts within %d bytes each", len(parts), peernetwork.MaxGrantBytes)
}
