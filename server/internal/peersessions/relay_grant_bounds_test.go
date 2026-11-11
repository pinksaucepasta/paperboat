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

// The largest grant the server can chunk, signed by mint, fits the relay's token bound: the chunk bounds (peers,
// scopes) are the relay's own, and every scope carries a resource id of the longest length a scope may have.
func TestTheLargestRelayGrantChunkFitsTheRelayTokenBound(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	provider, err := mint.New([]mint.Key{{ID: "k", PrivateKey: ed25519.NewKeyFromSeed(seed)}}, "k", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	key := func(i int) string { return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%032d", i)) }
	self := networkBinding{AccountID: strings.Repeat("a", 256), EndpointID: strings.Repeat("e", 256), WireGuardPublicKey: key(0)}
	var peers []networkPeer
	for i := 1; i <= 3*peernetwork.MaxGrantPeers; i++ {
		var scopes []networkScope
		for c := range 4 {
			scopes = append(scopes, networkScope{ResourceKind: "machine_access", ResourceID: strings.Repeat(fmt.Sprint(c), peernetwork.MaxResourceIDBytes),
				ResourceGeneration: 1 << 53, Capability: "file_transfer", Direction: "accept", Port: peernetwork.NetworkPort, ExpiresAt: 1 << 40})
		}
		peers = append(peers, networkPeer{Identity: networkBinding{AccountID: self.AccountID, EndpointID: fmt.Sprint(i), WireGuardPublicKey: key(i), DiscoPublicKey: key(i)}, Scopes: scopes})
	}
	now := time.Now().Unix()
	for _, chunk := range relayGrantChunks(self, peers, maximumRelayGrantPeers, now+60) {
		grant := peernetwork.RelayGrant{Version: 1, Issuer: strings.Repeat("i", 256), Audience: peernetwork.RelayGrantAudience, IssuedAt: now, ExpiresAt: now + 60, Generation: 1 << 53,
			AccountID: self.AccountID, EndpointID: self.EndpointID, WireGuardPublicKey: self.WireGuardPublicKey, DiscoPublicKey: key(0), CertificateFingerprint: strings.Repeat("f", 64),
			QUICPublicKey: key(0), NodeID: strings.Repeat("n", 256), NodeGeneration: 1 << 53, ProcessEpoch: strings.Repeat("p", 256), Peers: chunk.peers, RelayControlPeers: chunk.control}
		token, err := provider.SignRelayGrant(grant)
		if err != nil {
			t.Fatal(err)
		}
		if len(token) > peernetwork.MaxGrantBytes {
			t.Fatalf("a %d-peer grant chunk signs to %d bytes, over the relay's %d", len(chunk.peers), len(token), peernetwork.MaxGrantBytes)
		}
	}
}
